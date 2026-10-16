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
		MaxIdleConns:        20,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 5,
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
func zenChatStream(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+zenHost+zenChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// A fresh descending session id per attempt: reusing one across retries is
	// what makes the upstream answer a spurious "outside OpenCode" 403.
	req.Header = zenHeaders(body, ocSessionID())
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream transport: %w", err)
	}
	return resp, nil
}
