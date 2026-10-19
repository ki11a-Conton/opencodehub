// httpclient.go — the outbound HTTP client for opencode.ai.
//
// opencode.ai is only reachable from this machine through the local Clash
// Verge mixed port, so the client always dials through it. Two properties
// matter for the "proxy off must not break anything" requirement:
//
//   - the dial has a hard 3 s timeout, so a dead proxy fails fast instead of
//     hanging the caller;
//   - nothing here is called during registration, so a dead proxy cannot slow
//     CPA startup down at all.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultLocalProxy is the machine-local proxy (Clash Verge mixed port).
const defaultLocalProxy = "http://127.0.0.1:7897"

// proxyEnvVar overrides the outbound proxy for this plugin only. It exists so
// the operator (and the verification runs) can point the plugin at a different
// port, or at "" for a direct connection, without editing the binary or
// touching the machine's real Clash Verge instance.
const proxyEnvVar = "OPENCODEHUB_PROXY"

// localProxy holds the outbound proxy URL. It is read on every dial (inside the
// transport's Proxy callback) and is therefore stored atomically: the transport
// is built once, while a test may repoint it at a dead port at any time.
var localProxy atomic.Value

func init() {
	proxy := strings.TrimSpace(os.Getenv(proxyEnvVar))
	if proxy == "" {
		proxy = defaultLocalProxy
	} else if proxy == "direct" || proxy == "none" || proxy == "off" {
		// Explicit opt-out: no proxy at all.
		proxy = ""
	}
	localProxy.Store(proxy)
}

// proxyURL returns the proxy every outbound request must go through, or "" when
// the operator asked for a direct connection.
func proxyURL() string {
	if v, ok := localProxy.Load().(string); ok {
		return v
	}
	return defaultLocalProxy
}

// proxyFunc resolves the proxy per request. Returning (nil, nil) means "connect
// directly", which is how OPENCODEHUB_PROXY=direct is honoured.
func proxyFunc(*http.Request) (*url.URL, error) {
	raw := proxyURL()
	if raw == "" {
		return nil, nil
	}
	return url.Parse(raw)
}

// setLocalProxy repoints outbound traffic. Tests use it to simulate a
// switched-off Clash Verge without touching the real proxy.
func setLocalProxy(u string) { localProxy.Store(u) }

// transportLabel describes the egress path for humans (management panel, logs).
func transportLabel() string {
	if raw := proxyURL(); raw != "" {
		return raw
	}
	return "direct (no proxy)"
}

const (
	// proxyDialTimeout bounds the TCP connect through the local proxy.
	proxyDialTimeout = 3 * time.Second
	// zenRequestTimeout bounds a whole chat request. Streaming responses are
	// long-lived by nature, so this only guards the header phase plus stalls.
	zenRequestTimeout = 600 * time.Second
	// zenHeaderTimeout bounds the wait from "request written" to "response
	// headers received" — the phase where the free tier stalls instead of
	// answering. A slow-but-alive model (big-pickle needs ~36 s end to end)
	// returns its headers within ~2 s and then streams, so this never cuts a
	// healthy stream short; a stalled request returns no headers at all.
	// Measured: healthy first-byte is 1.2-4 s, so 10 s leaves 2.5x headroom
	// while keeping the worst case (3 attempts) near 30 s instead of 75 s.
	zenHeaderTimeout = 10 * time.Second
	// zenMaxAttempts is the total number of upstream attempts per client call.
	// The free tier's stalls are intermittent (measured 8/10 clean, 2/10 hanging
	// to the client timeout), so a fresh attempt almost always lands; without
	// retries a single stall surfaces to the client as a 90 s timeout.
	zenMaxAttempts = 3
	// zenRetryBackoff is the base delay between attempts (grows linearly).
	zenRetryBackoff = 400 * time.Millisecond
)

var (
	clientOnce        sync.Once
	client            *http.Client
	catalogClientOnce sync.Once
	catalogClient     *http.Client
)

// proxiedTransport returns a transport that always goes through the local
// proxy, resolved per request so a repointed proxy takes effect immediately.
func proxiedTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: proxyFunc,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		// Applied to the chat client only (see httpClient); the catalog client
		// is cloned from this transport and re-sets it to its own short budget.
		ResponseHeaderTimeout: zenHeaderTimeout,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   5,
		// The upstream contract asserts an uncompressed body; never let the
		// transport negotiate gzip behind our back.
		DisableCompression: true,
	}
}

// httpClient returns the shared client used for chat traffic.
func httpClient() *http.Client {
	clientOnce.Do(func() {
		client = &http.Client{Timeout: zenRequestTimeout, Transport: proxiedTransport()}
	})
	return client
}

// catalogHTTPClient returns the client used for the model catalog. It shares the
// proxied transport (never http.DefaultTransport, which would silently dial
// opencode.ai directly and ignore localProxy) but keeps a short timeout so a
// switched-off proxy fails fast instead of pinning a background goroutine.
func catalogHTTPClient() *http.Client {
	catalogClientOnce.Do(func() {
		catalogClient = &http.Client{Timeout: catalogRefreshTimeout, Transport: proxiedTransport()}
	})
	return catalogClient
}

// zenGet performs an anonymous GET against the Zen API (model catalog).
func zenGet(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, "https://"+zenHost+path, nil)
	if err != nil {
		return nil, err
	}
	// The catalog endpoint needs no OpenCode session headers, only the shared
	// identity, so a session-less request is used here.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", openCodeUserAgent)
	resp, err := catalogHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog transport: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("catalog read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog http %d: %s", resp.StatusCode, truncateBody(body))
	}
	return body, nil
}

// zenChatStream posts a prepared chat body and returns the live response. The
// caller owns the body and must close it (in the streaming path: from the
// goroutine that pumps it, never with a deferred close in the handler).
//
// The failure mode this guards is a stall: the free tier accepts the request
// and then never sends response headers (measured 2 stalls in 10 calls, each
// hanging to the client's 90 s timeout before this existed). Each attempt
// rebuilds the whole request with a fresh session and message id, so a retry is
// never a byte-identical replay of the attempt that just failed.
func zenChatStream(ctx context.Context, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= zenMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+zenHost+zenChatPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = zenHeaders(body, ocSessionID())
		resp, err := httpClient().Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if err == nil {
			// Drain and close so the connection returns to the pool; keep the
			// upstream's own wording, the client deserves the real reason.
			rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncateBody(rawErr))
			if !retryableStatus(resp.StatusCode) {
				return nil, lastErr
			}
		} else {
			lastErr = fmt.Errorf("upstream transport: %w", err)
			// A cancelled client request is not worth retrying.
			if ctx.Err() != nil {
				return nil, lastErr
			}
		}
		if attempt < zenMaxAttempts {
			select {
			case <-time.After(zenRetryBackoff * time.Duration(attempt)):
			case <-ctx.Done():
				return nil, lastErr
			}
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", zenMaxAttempts, lastErr)
}

// retryableStatus reports whether another attempt can plausibly succeed.
// 403 is the free tier's "only from within OpenCode" identity check. This plugin
// should never earn one — declaring the `read` tool plus the x-opencode-* header
// family is what satisfies that gate (see README) — so a 403 arriving here means
// either the body was rewritten in a way that dropped `read`, or upstream moved
// the goalposts. A single fresh attempt is cheap in both cases. 429 and 5xx are
// transient by definition. A 400/404/410 means the request or the model itself
// is wrong: retrying would only burn the client's time.
func retryableStatus(code int) bool {
	switch {
	case code == http.StatusForbidden:
		return true
	case code == http.StatusTooManyRequests:
		return true
	case code >= 500:
		return true
	default:
		return false
	}
}
