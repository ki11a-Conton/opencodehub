// models.go — the Zen model catalog exposed to the host.
//
// Two hard rules shape this file:
//
//  1. Registration must never touch the network. The host calls StaticModels
//     while it is starting up; a dial that blocks (or an upstream that is
//     unreachable because the user's proxy is off) must not delay or fail CPA
//     startup. So StaticModels returns the compiled-in list immediately and,
//     at most, kicks off a background refresh whose result lands in a cache
//     that a LATER call can pick up.
//
//  2. Only free-tier models are advertised. The upstream /zen/v1/models list
//     also contains paid claude-*/gpt-*/gemini-* ids that the anonymous
//     "Bearer public" identity cannot use; publishing them would only produce
//     403s in the client's face.
package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// zenFreeModel is one advertised model: the upstream id plus display metadata.
type zenFreeModel struct {
	ID          string
	DisplayName string
	Context     int64
	MaxOutput   int64
}

// zenFreeModels is the compiled-in free-tier catalog: the ids that are
// reachable with the anonymous free-tier identity and that the upstream
// /zen/v1/models endpoint currently lists. It mirrors DEFAULT_MODELS in the
// upstream gateway's src/models/catalog.ts, minus the entries that were
// retired or that only exist behind a paid subscription.
var zenFreeModels = []zenFreeModel{
	{ID: "big-pickle", DisplayName: "Big Pickle", Context: 262144, MaxOutput: 32768},
	{ID: "cat-jam-free", DisplayName: "Cat Jam (free)", Context: 262144, MaxOutput: 32768},
	{ID: "mimo-v2.6-flash-free", DisplayName: "MiMo v2.6 Flash (free)", Context: 262144, MaxOutput: 32768},
	{ID: "deepseek-v4.1-flash-free", DisplayName: "DeepSeek v4.1 Flash (free)", Context: 262144, MaxOutput: 32768},
	{ID: "nemotron-3.5-lightning-free", DisplayName: "Nemotron 3.5 Lightning (free)", Context: 262144, MaxOutput: 32768},
	{ID: "nemotron-3.5-super-vl-free", DisplayName: "Nemotron 3.5 Super VL (free)", Context: 262144, MaxOutput: 32768},
	{ID: "muse-spark-1.2-contributor-free", DisplayName: "Muse Spark 1.2 (free)", Context: 262144, MaxOutput: 32768},
	{ID: "muse-spark-1.3-contributor-free", DisplayName: "Muse Spark 1.3 (free)", Context: 262144, MaxOutput: 32768},
	{ID: "step-5-preview-free", DisplayName: "Step 5 Preview (free)", Context: 262144, MaxOutput: 32768},
	{ID: "longcat-2.5-preview-free", DisplayName: "LongCat 2.5 Preview (free)", Context: 262144, MaxOutput: 32768},
	{ID: "space-bunny-free", DisplayName: "Space Bunny (free)", Context: 262144, MaxOutput: 32768},
	{ID: "jev-1.13-free", DisplayName: "Jev 1.13 (free)", Context: 262144, MaxOutput: 32768},
}

// zenModelSet is the lookup used by the router to decide whether a request
// belongs to this plugin.
var zenModelSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(zenFreeModels))
	for _, m := range zenFreeModels {
		out[m.ID] = struct{}{}
	}
	return out
}()

// isZenModel reports whether id (in any client-facing spelling) names one of
// the models this plugin owns.
//
// Ownership must cover everything model.static publishes, not just the
// compiled-in list: a free id discovered by a catalog refresh is advertised to
// clients, and if the router did not claim it the request would fall through to
// the host's auth selector and fail with "auth_unavailable: no auth available".
func isZenModel(id string) bool {
	normalized := normalizeUpstreamModel(id)
	if normalized == "" {
		return false
	}
	if _, ok := zenModelSet[normalized]; ok {
		return true
	}
	// Consult the cached catalog only — never schedule a refresh from the
	// routing hot path.
	catalogMu.Lock()
	cached := catalogModels
	catalogMu.Unlock()
	for _, m := range cached {
		if m.ID == normalized {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Catalog cache
// -----------------------------------------------------------------------------

const (
	// catalogTTL bounds how long a live catalog is reused.
	catalogTTL = 10 * time.Minute
	// catalogRefreshTimeout keeps a background refresh short enough that a
	// dead proxy cannot pile up goroutines.
	catalogRefreshTimeout = 6 * time.Second
	// catalogLogInterval rate-limits "catalog refresh failed" log lines.
	catalogLogInterval = 30 * time.Second
)

var (
	catalogMu      sync.Mutex
	catalogModels  []zenFreeModel
	catalogFetched time.Time
	catalogLoading bool

	catalogLogMu sync.Mutex
	catalogLogAt time.Time
)

// logCatalogFailure emits at most one catalog warning per catalogLogInterval,
// so a proxy that is off cannot flood the host log at every registration.
func logCatalogFailure(err error) {
	catalogLogMu.Lock()
	defer catalogLogMu.Unlock()
	if time.Since(catalogLogAt) < catalogLogInterval {
		return
	}
	catalogLogAt = time.Now()
	log.Printf("opencodehub catalog: refresh failed: %v (serving the compiled-in free list)", err)
}

// currentModels returns the model list to publish: the cached live catalog
// when one is fresh, otherwise the compiled-in list. It never blocks on the
// network; a stale-or-missing cache schedules a background refresh instead.
func currentModels() []zenFreeModel {
	catalogMu.Lock()
	cached := catalogModels
	fresh := len(cached) > 0 && time.Since(catalogFetched) < catalogTTL
	needRefresh := !fresh && !catalogLoading
	if needRefresh {
		catalogLoading = true
	}
	catalogMu.Unlock()

	if needRefresh {
		go refreshCatalog()
	}
	if len(cached) > 0 {
		return cached
	}
	return zenFreeModels
}

// refreshCatalog fetches /zen/v1/models and stores the free-tier subset.
// Failures are logged (rate-limited) and leave the compiled-in list in place.
func refreshCatalog() {
	defer func() {
		catalogMu.Lock()
		catalogLoading = false
		catalogMu.Unlock()
	}()

	body, err := zenGet(zenModelsPath)
	if err != nil {
		logCatalogFailure(err)
		return
	}
	models, err := parseZenCatalog(body)
	if err != nil {
		logCatalogFailure(err)
		return
	}
	if len(models) == 0 {
		return
	}
	catalogMu.Lock()
	catalogModels = models
	catalogFetched = time.Now()
	catalogMu.Unlock()
}

// parseZenCatalog extracts the free-tier ids from a /zen/v1/models payload.
// Newly released free-tier ids are appended to the compiled-in list so they
// show up without a rebuild; ids that disappear upstream are kept, because an
// upstream outage must never shrink the published list.
func parseZenCatalog(raw []byte) ([]zenFreeModel, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]zenFreeModel, 0, len(zenFreeModels)+4)
	seen := map[string]bool{}
	for _, m := range zenFreeModels {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || !isFreeTierID(id) {
			continue
		}
		seen[id] = true
		out = append(out, zenFreeModel{
			ID:          id,
			DisplayName: displayNameFor(id),
			Context:     262144,
			MaxOutput:   32768,
		})
	}
	return out, nil
}

// isFreeTierID reports whether an upstream id belongs to the anonymous free
// tier: the "-free" suffix, or the one free flagship without it.
func isFreeTierID(id string) bool {
	return id == "big-pickle" || strings.HasSuffix(id, "-free")
}

// displayNameFor turns an id into a readable label.
func displayNameFor(id string) string {
	trimmed := strings.TrimSuffix(id, "-free")
	parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	name := strings.Join(parts, " ")
	if strings.HasSuffix(id, "-free") {
		name += " (free)"
	}
	return name
}

// -----------------------------------------------------------------------------
// Host-facing model list
// -----------------------------------------------------------------------------

// handleModelStatic answers model.static. The host calls it while registering
// plugins, so it must return promptly and must never fail: the compiled-in
// list is always available, and any network refresh happens in the background.
func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	excluded := excludedModelSet(req.Host.ExcludedModels[providerName])
	models := make([]pluginapi.ModelInfo, 0, len(zenFreeModels))
	for _, m := range currentModels() {
		if _, skip := excluded[m.ID]; skip {
			continue
		}
		models = append(models, modelInfoFor(m))
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models}), nil
}

// handleModelForAuth answers model.for_auth. This plugin owns no auth records
// (the Zen free tier is anonymous), so the answer is always an empty set.
func handleModelForAuth(raw []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName}), nil
}

// modelInfoFor maps a catalog entry onto the host's ModelInfo schema.
func modelInfoFor(m zenFreeModel) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		OwnedBy:                    providerName,
		Type:                       "chat",
		DisplayName:                m.DisplayName,
		Name:                       m.ID,
		Description:                "OpenCode Zen free tier via " + providerName,
		InputTokenLimit:            m.Context,
		OutputTokenLimit:           m.MaxOutput,
		ContextLength:              m.Context,
		MaxCompletionTokens:        m.MaxOutput,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedParameters:        []string{"tools", "tool_choice", "temperature", "top_p", "max_tokens", "stream"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
	}
}

// excludedModelSet lowercases a host exclusion list for set membership.
func excludedModelSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out[n] = struct{}{}
		}
	}
	return out
}
