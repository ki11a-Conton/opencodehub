// zen.go — OpenCode Zen wire protocol (https://opencode.ai/zen/v1/...).
//
// Ported from ki11a-Conton/OpenCodeProxyHub (MIT), which derives from
// bigdata2211it-web/opencode-free-proxy:
//
//	src/providers/opencodeHeaders.ts  -> zenHeaders
//	src/utils/ids.ts                  -> ocID / ocSessionID
//	src/providers/zenClient.ts        -> prepareZenChatBody
//
// The Zen free tier is anonymous: Authorization is the literal string
// "Bearer public" and access is gated on the *shape and direction* of the
// session id, not on any credential. The upstream validates that
// x-opencode-session / x-opencode-session-id carry a DESCENDING timestamp
// (SessionID.create() packs ~(ms*0x1000+counter) into six big-endian bytes);
// an ascending or zero timestamp is rejected with HTTP 403 FreeTierError
// ("OpenCode's free tier can only be used from within OpenCode").
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// zenHost is the upstream host; port 443 is implicit (https scheme).
	zenHost = "opencode.ai"
	// zenChatPath is the OpenAI-shaped chat completions endpoint.
	zenChatPath = "/zen/v1/chat/completions"
	// zenModelsPath lists every model the upstream knows (free and paid).
	zenModelsPath = "/zen/v1/models"

	// openCodeUserAgent is the identity of @opencode/cli 2.0.29, the release
	// the header set was verified against.
	openCodeUserAgent = "opencode/latest/2.0.29/cli"
	// zenAuthHeader is the anonymous free-tier bearer token.
	zenAuthHeader = "Bearer public"

	// errorBodyLimit caps how much of an upstream error body is echoed back.
	errorBodyLimit = 4096
)

// base62 is the alphabet used for the random tail of OpenCode identifiers.
const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	ocIDMu      sync.Mutex
	ocIDLastTS  int64
	ocIDCounter uint64
)

// ocID mirrors Identifier.create() from @opencode/cli: the time component is
// (unixMilli * 0x1000 + counter) packed into six big-endian bytes, followed by
// 14 base62 random characters. Session ids are descending (~packed), message
// and tool ids ascend.
func ocID(prefix string, descending bool) string {
	ocIDMu.Lock()
	now := time.Now().UnixMilli()
	if now != ocIDLastTS {
		ocIDLastTS = now
		ocIDCounter = 0
	}
	ocIDCounter++
	counter := ocIDCounter
	ocIDMu.Unlock()

	packed := uint64(now)*0x1000 + counter
	if descending {
		packed = ^packed
	}
	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte((packed >> uint(40-8*i)) & 0xff)
	}
	rnd := make([]byte, 14)
	// crypto/rand.Read never fails on supported platforms; a zero-filled tail
	// would still be a syntactically valid id.
	_, _ = rand.Read(rnd)
	for i := range rnd {
		rnd[i] = base62[int(rnd[i])%62]
	}
	return prefix + "_" + hex.EncodeToString(timeBytes[:]) + string(rnd)
}

// ocSessionID returns a fresh descending session id. Every upstream attempt
// must use a new one: reusing a session across proxy rotations is what makes
// the upstream answer a spurious "outside OpenCode" 403.
func ocSessionID() string { return ocID("ses", true) }

// zenHeaders builds the ten headers the upstream expects, verbatim from
// createOpenCodeHeaders(). Content-Length is set explicitly from the UTF-8
// byte length and Accept-Encoding is pinned to "identity" so Go's transport
// never negotiates transparent gzip (the upstream contract asserts an
// uncompressed body).
func zenHeaders(body []byte, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Accept-Encoding", "identity")
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Authorization", zenAuthHeader)
	h.Set("User-Agent", openCodeUserAgent)
	h.Set("x-opencode-client", "cli")
	h.Set("x-opencode-project", "global")
	h.Set("x-opencode-request", ocID("msg", false))
	h.Set("x-opencode-session", sessionID)
	h.Set("x-opencode-session-id", sessionID)
	return h
}

// zenChatBody rewrites a downstream chat-completions body into the shape the
// Zen chat endpoint accepts:
//
//   - model is replaced by the resolved upstream id;
//   - stream is forced to true (the upstream rejects non-streaming requests);
//   - tools always carry lowercase "read" and "bash" (see tools.go);
//   - every other client parameter is forwarded as-is, except the ones we own.
//
// It returns the encoded body plus the mapper needed to translate the response
// back into the client's own tool spelling.
func zenChatBody(raw []byte, upstreamModel string) ([]byte, *toolMapper, error) {
	obj, ok := parseObjectBody(raw)
	if !ok {
		// Not a JSON object: hand the bytes through untouched so the upstream
		// produces a meaningful protocol error instead of a plugin 500.
		return raw, newToolMapper(nil), nil
	}
	if upstreamModel != "" {
		obj["model"] = upstreamModel
	}
	obj["stream"] = true

	mapper := newToolMapper(obj["tools"])
	obj["tools"] = mapper.upstreamToolList(obj["tools"])

	if tc, present := obj["tool_choice"]; present {
		obj["tool_choice"] = mapper.upstreamToolChoice(tc)
	}
	if msgs, ok := obj["messages"].([]any); ok {
		obj["messages"] = mapper.upstreamMessages(msgs)
	}

	// Drop parameters that belong to the downstream protocol only.
	for _, key := range []string{"stream_options", "response_format", "user", "seed", "stop"} {
		delete(obj, key)
	}
	return mustJSON(obj), mapper, nil
}

// parseObjectBody decodes raw into a generic JSON object.
func parseObjectBody(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// mustJSON encodes v, falling back to an empty object when marshalling fails
// (only reachable for values this package never produces).
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// normalizeUpstreamModel maps any client-facing spelling of a Zen model id
// back to the bare id the upstream accepts: "opencodehub/big-pickle" and
// "opencodehub-big-pickle" both become "big-pickle", a bare id passes through.
func normalizeUpstreamModel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, providerName+"/") {
		s = strings.TrimPrefix(s, providerName+"/")
	} else if strings.HasPrefix(s, providerName+"-") {
		s = strings.TrimPrefix(s, providerName+"-")
	}
	return strings.TrimSpace(s)
}

// truncateBody renders an upstream error body for a log line / error message.
func truncateBody(body []byte) string {
	s := strings.ReplaceAll(string(body), "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
