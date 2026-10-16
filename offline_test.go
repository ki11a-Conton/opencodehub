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
