// offline_test.go — proves the plugin cannot be affected by a dead proxy.
//
// The user requirement this file guards: starting CPA with the local proxy
// switched off must be exactly as clean as starting it with the proxy on. The
// plugin therefore performs no network I/O while registering — model.static
// answers from a compiled-in list and any catalog refresh happens in a
// background goroutine. These tests point the outbound client at a dead port
// (the local proxy is a var precisely so tests can do this) and assert that the
// registration path still answers, quickly, with the full model list.
package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// deadProxyPort has no listener, so every dial is refused immediately — the
// same failure mode as a switched-off Clash Verge.
const deadProxyPort = "http://127.0.0.1:59999"

// withDeadProxy repoints the outbound client at the dead port for one test and
// restores the real port afterwards.
func withDeadProxy(t *testing.T) {
	t.Helper()
	original := proxyURL()
	setLocalProxy(deadProxyPort)
	t.Cleanup(func() { setLocalProxy(original) })
}

// TestStaticModelsAnswerWithProxyOff is the core guarantee: model.static must
// return the compiled-in list promptly even when every outbound dial fails.
func TestStaticModelsAnswerWithProxyOff(t *testing.T) {
	withDeadProxy(t)

	start := time.Now()
	raw, err := handleModelStatic(nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handleModelStatic failed with the proxy off: %v", err)
	}
	// The answer is served from memory; anything close to a dial timeout means
	// the registration path is waiting on the network and would slow CPA down.
	if elapsed > time.Second {
		t.Fatalf("handleModelStatic took %s with the proxy off, want a non-blocking answer", elapsed)
	}

	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string `json:"Provider"`
			Models   []struct {
				ID string `json:"ID"`
			} `json:"Models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode static response: %v", err)
	}
	if !env.OK || env.Result.Provider != providerName {
		t.Fatalf("unexpected static response: %s", raw)
	}
	if len(env.Result.Models) != len(zenFreeModels) {
		t.Fatalf("published %d models with the proxy off, want the full compiled list of %d",
			len(env.Result.Models), len(zenFreeModels))
	}
	ids := map[string]bool{}
	for _, m := range env.Result.Models {
		ids[m.ID] = true
	}
	if !ids["big-pickle"] {
		t.Fatalf("big-pickle missing from the offline list: %v", ids)
	}
}

// TestModelForAuthAnswerWithProxyOff: the plugin owns no auth records, so
// model.for_auth must also answer instantly with the proxy down. (The host only
// calls it when an auth exists, but a stale auth file must not turn into a
// startup warning either.)
func TestModelForAuthAnswerWithProxyOff(t *testing.T) {
	withDeadProxy(t)
	start := time.Now()
	raw, err := handleModelForAuth(nil)
	if err != nil {
		t.Fatalf("handleModelForAuth failed with the proxy off: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handleModelForAuth took %s with the proxy off", elapsed)
	}
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode for_auth response: %v", err)
	}
	if !env.OK {
		t.Fatalf("for_auth response not ok: %s", raw)
	}
}

// TestCatalogRefreshFailureKeepsList: a failed background refresh must log (rate
// limited) and leave the published list untouched.
func TestCatalogRefreshFailureKeepsList(t *testing.T) {
	withDeadProxy(t)

	// Seed a "live" catalog, then let a refresh fail and confirm it survives.
	catalogMu.Lock()
	savedModels, savedAt := catalogModels, catalogFetched
	catalogModels = []zenFreeModel{{ID: "cached-model", DisplayName: "Cached", Context: 1000, MaxOutput: 100}}
	catalogFetched = time.Now()
	catalogMu.Unlock()
	t.Cleanup(func() {
		catalogMu.Lock()
		catalogModels, catalogFetched = savedModels, savedAt
		catalogMu.Unlock()
	})

	refreshCatalog() // dials the dead port; must not panic or clear the cache

	catalogMu.Lock()
	got := append([]zenFreeModel(nil), catalogModels...)
	catalogMu.Unlock()
	if len(got) != 1 || got[0].ID != "cached-model" {
		t.Fatalf("a failed refresh changed the catalog: %v", got)
	}
}

// TestLogCatalogFailureIsRateLimited: repeated failures must not flood the host
// log (a switched-off proxy would otherwise print on every registration).
func TestLogCatalogFailureIsRateLimited(t *testing.T) {
	catalogLogMu.Lock()
	savedAt := catalogLogAt
	catalogLogAt = time.Time{} // zero => the first call is allowed through
	catalogLogMu.Unlock()
	t.Cleanup(func() {
		catalogLogMu.Lock()
		catalogLogAt = savedAt
		catalogLogMu.Unlock()
	})

	logCatalogFailure(errDeadProxy)
	catalogLogMu.Lock()
	first := catalogLogAt
	catalogLogMu.Unlock()
	if first.IsZero() {
		t.Fatal("the first failure did not record a timestamp")
	}

	// A second failure inside the interval must be suppressed, i.e. it must not
	// move the timestamp forward.
	time.Sleep(5 * time.Millisecond)
	logCatalogFailure(errDeadProxy)
	catalogLogMu.Lock()
	second := catalogLogAt
	catalogLogMu.Unlock()
	if !second.Equal(first) {
		t.Fatalf("the rate limiter let a second line through: %s -> %s", first, second)
	}
}

// TestDirectProxyModeSkipsTheProxy: OPENCODEHUB_PROXY=direct must produce a
// transport that dials the origin itself, so an operator without Clash Verge can
// still use the plugin.
func TestDirectProxyModeSkipsTheProxy(t *testing.T) {
	original := proxyURL()
	setLocalProxy("")
	t.Cleanup(func() { setLocalProxy(original) })

	req, err := http.NewRequest(http.MethodGet, "https://"+zenHost+zenModelsPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	u, err := proxiedTransport().Proxy(req)
	if err != nil {
		t.Fatalf("proxy resolution failed: %v", err)
	}
	if u != nil {
		t.Fatalf("direct mode still used the proxy %v", u)
	}
}

// TestProxyOverrideIsUsed: a non-empty override must be honoured verbatim.
func TestProxyOverrideIsUsed(t *testing.T) {
	original := proxyURL()
	setLocalProxy("http://127.0.0.1:12345")
	t.Cleanup(func() { setLocalProxy(original) })

	req, _ := http.NewRequest(http.MethodGet, "https://"+zenHost, nil)
	u, err := proxiedTransport().Proxy(req)
	if err != nil {
		t.Fatalf("proxy resolution failed: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:12345" {
		t.Fatalf("override not honoured, got %v", u)
	}
}

// errDeadProxy is the error shape a refused dial produces.
var errDeadProxy = errString("dial tcp 127.0.0.1:59999: connectex: connection refused")

type errString string

func (e errString) Error() string { return string(e) }

// --- free tier IP block: the egress fallback ------------------------------
//
// The free tier refuses whole address ranges, not credentials: a Clash exit on
// AS31898 answers every free model with 403 FreeTierError while the same request
// from the local ISP address answers 200 in ~2.4 s. These tests cover the three
// decisions that fallback is made of — recognising the body, suspending the
// proxy, and expiring the suspension — without touching the network.

// TestFreeTierBodyDetection: both upstream variants must be recognised, and an
// unrelated 403 must not be.
func TestFreeTierBodyDetection(t *testing.T) {
	fromWithin := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode."}}`)
	fromAddress := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free models aren't available from your current network because we've detected abusive traffic from this IP address."}}`)
	other := []byte(`{"error":{"type":"AuthError","message":"invalid key"}}`)

	if !isFreeTierBody(fromWithin) {
		t.Error("the identity variant was not recognised")
	}
	if !isFreeTierBody(fromAddress) {
		t.Error("the address variant was not recognised")
	}
	if isFreeTierBody(other) {
		t.Error("an unrelated error was mistaken for a free tier block")
	}
}

// TestFreeTierBlockSuspendsTheProxy: once marked, no dial may go through the
// proxy — not through the transport callback, and not through the client the
// chat path picks.
func TestFreeTierBlockSuspendsTheProxy(t *testing.T) {
	original := proxyURL()
	setLocalProxy(deadProxyPort)
	clearProxyBlock()
	t.Cleanup(func() {
		setLocalProxy(original)
		clearProxyBlock()
	})

	if !dialsThroughProxy() {
		t.Fatal("the proxy should be in use before any block")
	}

	markProxyBlocked(freeTierCooldown, "test block")

	if dialsThroughProxy() {
		t.Fatal("the proxy was still in use after a free tier block")
	}
	req, err := http.NewRequest(http.MethodPost, "https://"+zenHost+zenChatPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	u, err := proxiedTransport().Proxy(req)
	if err != nil {
		t.Fatalf("proxy resolution failed: %v", err)
	}
	if u != nil {
		t.Fatalf("the transport still dials the proxy %v after a block", u)
	}
	if got := chatClientFor(); got != directHTTPClient() {
		t.Fatal("the chat path did not switch to the direct client")
	}
}

// TestProxyBlockExpires: the suspension must lift on its own, otherwise one bad
// minute would pin the operator to direct mode for the rest of the process.
func TestProxyBlockExpires(t *testing.T) {
	original := proxyURL()
	setLocalProxy(deadProxyPort)
	clearProxyBlock()
	t.Cleanup(func() {
		setLocalProxy(original)
		clearProxyBlock()
	})

	markProxyBlocked(15*time.Millisecond, "test block")
	if !proxyEgressBlocked() {
		t.Fatal("the block was not applied")
	}
	time.Sleep(40 * time.Millisecond)
	if proxyEgressBlocked() {
		t.Fatal("the block did not expire")
	}
	if !dialsThroughProxy() {
		t.Fatal("the proxy did not come back after the block expired")
	}
}

// TestDirectModeIsUnaffectedByBlocks: OPENCODEHUB_PROXY=direct means there is no
// proxy to suspend, so the fallback bookkeeping must stay out of the way.
func TestDirectModeIsUnaffectedByBlocks(t *testing.T) {
	original := proxyURL()
	setLocalProxy("")
	clearProxyBlock()
	t.Cleanup(func() {
		setLocalProxy(original)
		clearProxyBlock()
	})

	markProxyBlocked(freeTierCooldown, "test block")
	if dialsThroughProxy() {
		t.Fatal("direct mode started using a proxy")
	}
	if got := chatClientFor(); got != directHTTPClient() {
		t.Fatal("direct mode did not use the direct client")
	}
}
