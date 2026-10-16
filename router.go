// router.go — the model router.
//
// Without this file the models registered by models.go would be resolved by
// the host's auth selector, which finds no auth record for provider
// "opencodehub" and answers the client with
// "auth_unavailable: no auth available" (HTTP 503). The router runs BEFORE
// model-to-provider resolution and auth selection, so claiming the request
// here is what makes an auth-less plugin usable.
package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleModelRoute answers model.route. It claims exactly the models this
// plugin publishes and hands them to its own executor.
func handleModelRoute(raw []byte) ([]byte, error) {
	var req pluginapi.ModelRouteRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return errorEnvelope("invalid_request", "model.route: "+err.Error()), nil
		}
	}
	model := normalizeUpstreamModel(req.RequestedModel)
	if model == "" || !isZenModel(model) {
		// Not ours: stay out of the way so another plugin or a built-in
		// provider can handle it.
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false}), nil
	}
	// An image-only or unsupported format still belongs to us; the executor
	// reports the protocol error, which keeps the failure message accurate.
	if !routeFormatSupported(req.SourceFormat) {
		return okEnvelope(pluginapi.ModelRouteResponse{
			Handled:    true,
			TargetKind: pluginapi.ModelRouteTargetSelf,
			Reason:     "opencodehub serves chat-completions only",
		}), nil
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetSelf,
		TargetModel: model,
		Reason:      "opencodehub free-tier model",
	}), nil
}

// routeFormatSupported reports whether the inbound protocol is one this plugin
// can serve. An empty format means the host did not say, which is treated as
// supported (chat-completions is the default).
func routeFormatSupported(sourceFormat string) bool {
	f := strings.ToLower(strings.TrimSpace(sourceFormat))
	if f == "" {
		return true
	}
	switch f {
	case "chat-completions", "chat_completions", "openai-chat-completions",
		"openai_chat_completions", "openai", "chat":
		return true
	default:
		return false
	}
}
