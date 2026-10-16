// registration_test.go — drives the real RPC dispatch table the way the host
// does, so the "proxy off" guarantee is verified at the same entry point CPA
// uses (cliproxyPluginCall -> handleMethod), not just at the helper level.
package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// callRPC is a thin helper: invoke handleMethod and fail on a transport error.
func callRPC(t *testing.T, method string, request []byte) []byte {
	t.Helper()
	raw, err := handleMethod(method, request)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return raw
}

// decodeEnvelope unwraps the {ok,result,error} RPC envelope.
func decodeEnvelope(t *testing.T, method string, raw []byte) (map[string]any, bool) {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s: decode envelope: %v (%s)", method, err, raw)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, false
		}
		return nil, false
	}
	var result map[string]any
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &result); err != nil {
			t.Fatalf("%s: decode result: %v (%s)", method, err, env.Result)
		}
	}
	return result, true
}

// TestRegistrationSequenceWithProxyOff replays the exact RPC sequence the host
// issues while loading plugins — plugin.register, model.static, then the
// identifier lookups — with every outbound dial refused. CPA startup must not
// depend on the local proxy being up, so the whole sequence has to answer from
// memory within a small budget.
func TestRegistrationSequenceWithProxyOff(t *testing.T) {
	withDeadProxy(t)

	start := time.Now()

	// 1. plugin.register / plugin.reconfigure: declares the capability set.
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		result, ok := decodeEnvelope(t, method, callRPC(t, method, nil))
		if !ok {
			t.Fatalf("%s returned a failure envelope", method)
		}
		caps, _ := result["capabilities"].(map[string]any)
		if caps == nil {
			t.Fatalf("%s: no capabilities in %v", method, result)
		}
		for _, want := range []string{"model_provider", "model_router", "executor"} {
			if caps[want] != true {
				t.Errorf("%s: capability %s = %v, want true", method, want, caps[want])
			}
		}
		if caps["executor_model_scope"] != "static" {
			t.Errorf("%s: executor_model_scope = %v, want static", method, caps["executor_model_scope"])
		}
	}

	// 2. model.static: the model list the host publishes. Must be complete and
	//    instant, because it is answered while CPA is still starting up.
	staticResult, ok := decodeEnvelope(t, pluginabi.MethodModelStatic, callRPC(t, pluginabi.MethodModelStatic, nil))
	if !ok {
		t.Fatal("model.static returned a failure envelope with the proxy off")
	}
	if staticResult["Provider"] != providerName {
		t.Errorf("model.static provider = %v, want %s", staticResult["Provider"], providerName)
	}
	models, _ := staticResult["Models"].([]any)
	if len(models) != len(zenFreeModels) {
		t.Fatalf("model.static published %d models with the proxy off, want %d",
			len(models), len(zenFreeModels))
	}

	// 3. executor.identifier: the provider name used for routing.
	ident, ok := decodeEnvelope(t, pluginabi.MethodExecutorIdentifier, callRPC(t, pluginabi.MethodExecutorIdentifier, nil))
	if !ok || ident["identifier"] != providerName {
		t.Fatalf("executor.identifier = %v, want %s", ident, providerName)
	}

	// 4. count_tokens: answered locally, never dials.
	if _, ok := decodeEnvelope(t, pluginabi.MethodExecutorCountTokens, callRPC(t, pluginabi.MethodExecutorCountTokens, []byte(`{}`))); !ok {
		t.Fatal("executor.count_tokens failed with the proxy off")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the registration sequence took %s with the proxy off; it must not wait on the network", elapsed)
	}
}

// TestRouterClaimsOnlyZenModels is the safety property that keeps this plugin
// from hijacking other providers: model.route must answer Handled=false for
// anything it does not own, so CPA falls through to the normal auth path.
func TestRouterClaimsOnlyZenModels(t *testing.T) {
	cases := []struct {
		model   string
		handled bool
	}{
		{"big-pickle", true},
		{"deepseek-v4.1-flash-free", true},
		{"opencodehub/big-pickle", true},
		{"gpt-5.5", false},
		{"workbuddy-bridge-deepseek-v4.1-flash", false},
		{"trae-glm-5.3", false},
		{"claude-opus-5", false},
		{"", false},
	}
	for _, tc := range cases {
		payload := mustJSON(map[string]any{
			"PluginID":       providerName,
			"SourceFormat":   "openai",
			"RequestedModel": tc.model,
			"Stream":         false,
			"Body":           mustJSON(map[string]any{"model": tc.model}),
		})
		result, ok := decodeEnvelope(t, pluginabi.MethodModelRoute, callRPC(t, pluginabi.MethodModelRoute, payload))
		if !ok {
			t.Fatalf("model.route failed for %q", tc.model)
		}
		handled, _ := result["Handled"].(bool)
		if handled != tc.handled {
			t.Errorf("model.route(%q).Handled = %v, want %v", tc.model, handled, tc.handled)
		}
		if handled {
			if result["TargetKind"] != "self" {
				t.Errorf("model.route(%q).TargetKind = %v, want self", tc.model, result["TargetKind"])
			}
		}
	}
}

// TestRouterClaimsDiscoveredModels: a free model that only a catalog refresh
// discovered is advertised by model.static, so the router has to claim it too.
// Otherwise the request would reach the auth selector, which has no auth record
// for this provider and answers 503 auth_unavailable.
func TestRouterClaimsDiscoveredModels(t *testing.T) {
	const discovered = "brand-new-model-free"

	catalogMu.Lock()
	savedModels, savedAt := catalogModels, catalogFetched
	catalogModels = []zenFreeModel{{ID: discovered, DisplayName: "Brand New (free)", Context: 1, MaxOutput: 1}}
	catalogFetched = time.Now()
	catalogMu.Unlock()
	t.Cleanup(func() {
		catalogMu.Lock()
		catalogModels, catalogFetched = savedModels, savedAt
		catalogMu.Unlock()
	})

	// The published list and the claimed set must agree.
	staticResult, ok := decodeEnvelope(t, pluginabi.MethodModelStatic, callRPC(t, pluginabi.MethodModelStatic, nil))
	if !ok {
		t.Fatal("model.static failed")
	}
	advertised := false
	if models, _ := staticResult["Models"].([]any); models != nil {
		for _, item := range models {
			if m, _ := item.(map[string]any); m != nil && m["ID"] == discovered {
				advertised = true
			}
		}
	}
	if !advertised {
		t.Fatalf("model.static did not advertise the discovered model %q", discovered)
	}

	payload := mustJSON(map[string]any{
		"PluginID":       providerName,
		"SourceFormat":   "openai",
		"RequestedModel": discovered,
		"Body":           mustJSON(map[string]any{"model": discovered}),
	})
	result, ok := decodeEnvelope(t, pluginabi.MethodModelRoute, callRPC(t, pluginabi.MethodModelRoute, payload))
	if !ok {
		t.Fatal("model.route failed")
	}
	if handled, _ := result["Handled"].(bool); !handled {
		t.Fatalf("the router did not claim the advertised model %q: %v", discovered, result)
	}
}

// TestUnknownMethodIsRejected keeps the dispatch table honest.
func TestUnknownMethodIsRejected(t *testing.T) {
	raw := callRPC(t, "not.a.method", nil)
	if _, ok := decodeEnvelope(t, "not.a.method", raw); ok {
		t.Fatal("an unknown method returned a success envelope")
	}
}

// TestManagementStatusReportsTransport guards a regression that shipped once:
// the status payload marshalled the atomic proxy holder itself instead of its
// string value, so the panel rendered "transport":{}. The field must always be a
// human-readable string.
func TestManagementStatusReportsTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proxy    string
		expected string
	}{
		{name: "proxy configured", proxy: "http://127.0.0.1:7897", expected: "http://127.0.0.1:7897"},
		{name: "direct mode", proxy: "", expected: "direct (no proxy)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := proxyURL()
			setLocalProxy(tc.proxy)
			t.Cleanup(func() { setLocalProxy(original) })

			// The panel resource and the JSON status route share one handler.
			payload := mustJSON(map[string]any{"Path": "/plugins/" + providerName + "/status"})
			result, ok := decodeEnvelope(t, pluginabi.MethodManagementHandle, callRPC(t, pluginabi.MethodManagementHandle, payload))
			if !ok {
				t.Fatal("management.handle failed")
			}

			body := result["Body"]
			if body == nil {
				t.Fatalf("management response carried no body: %v", result)
			}
			// pluginapi.ManagementResponse.Body is a []byte, which encoding/json
			// renders as base64; the host decodes it back the same way.
			encoded, ok := body.(string)
			if !ok {
				t.Fatalf("unexpected body type %T", body)
			}
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("body is not base64: %v", err)
			}
			var status map[string]any
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatalf("status body is not JSON: %v (%s)", err, raw)
			}

			got, _ := status["transport"].(string)
			if got != tc.expected {
				t.Fatalf("transport = %q, want %q", got, tc.expected)
			}
			if count, _ := status["count"].(float64); count != float64(len(currentModels())) {
				t.Fatalf("count = %v, want %d", status["count"], len(currentModels()))
			}
		})
	}
}
