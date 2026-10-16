// wire_test.go — unit tests for the Zen wire helpers and the tool gate.
package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestOcSessionIDShape checks the identifier encoding the upstream validates:
// prefix, 12 hex characters of descending timestamp, 14 base62 random chars.
func TestOcSessionIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := ocSessionID()
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "ses_") {
			t.Fatalf("session id %q lacks the ses_ prefix", id)
		}
		body := strings.TrimPrefix(id, "ses_")
		if len(body) != 12+14 {
			t.Fatalf("session id %q has body length %d, want 26", id, len(body))
		}
		for _, r := range body[:12] {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("session id %q has non-hex time bytes", id)
			}
		}
		for _, r := range body[12:] {
			if !strings.ContainsRune(base62, r) {
				t.Fatalf("session id %q has a non-base62 tail", id)
			}
		}
	}
}

// TestOcSessionIDDescending checks that a later session id sorts lower: the
// timestamp is stored inverted, which is exactly what the free-tier gate
// asserts (an ascending id is rejected with 403).
func TestOcSessionIDDescending(t *testing.T) {
	first := ocSessionID()
	time.Sleep(2 * time.Millisecond)
	second := ocSessionID()
	if !(first > second) {
		t.Fatalf("session ids are not descending: first=%q second=%q", first, second)
	}
}

// TestOcMessageIDAscending checks that request ids are not inverted.
func TestOcMessageIDAscending(t *testing.T) {
	first := ocID("msg", false)
	time.Sleep(2 * time.Millisecond)
	second := ocID("msg", false)
	if !(first < second) {
		t.Fatalf("message ids are not ascending: first=%q second=%q", first, second)
	}
}

// TestZenHeaders pins the header set the upstream gate expects.
func TestZenHeaders(t *testing.T) {
	body := []byte(`{"model":"big-pickle","stream":true}`)
	h := zenHeaders(body, "ses_deadbeef1234ABCDEFGHIJKLMN")
	want := map[string]string{
		"Accept-Encoding":       "identity",
		"Content-Type":          "application/json",
		"Content-Length":        "36",
		"Authorization":         "Bearer public",
		"User-Agent":            "opencode/latest/2.0.29/cli",
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    "ses_deadbeef1234ABCDEFGHIJKLMN",
		"x-opencode-session-id": "ses_deadbeef1234ABCDEFGHIJKLMN",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if req := h.Get("x-opencode-request"); !strings.HasPrefix(req, "msg_") {
		t.Errorf("x-opencode-request = %q, want a msg_ id", req)
	}
}

// TestChatBodyForcesStreamAndTools verifies the two request rewrites the
// upstream requires: stream must be true, and both gated tools must be present.
func TestChatBodyForcesStreamAndTools(t *testing.T) {
	raw := []byte(`{"model":"opencodehub/big-pickle","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	out, mapper, err := zenChatBody(raw, "big-pickle")
	if err != nil {
		t.Fatalf("zenChatBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if obj["stream"] != true {
		t.Errorf("stream = %v, want true", obj["stream"])
	}
	if obj["model"] != "big-pickle" {
		t.Errorf("model = %v, want big-pickle", obj["model"])
	}
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want exactly the two injected placeholders", obj["tools"])
	}
	names := map[string]bool{}
	for _, item := range tools {
		entry, _ := item.(map[string]any)
		fn, _ := entry["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	if !names["read"] || !names["bash"] {
		t.Fatalf("tools names = %v, want read and bash", names)
	}
	if mapper.declared("read") || mapper.declared("bash") {
		t.Fatal("mapper reports the placeholders as client-declared")
	}
}

// TestToolGateRenamesClientTools verifies that a client's Read/Bash are
// renamed upstream and NOT duplicated by placeholders.
func TestToolGateRenamesClientTools(t *testing.T) {
	raw := []byte(`{"model":"big-pickle","messages":[],"tools":[{"type":"function","function":{"name":"Read","parameters":{}}},{"type":"function","function":{"name":"Bash","parameters":{}}},{"type":"function","function":{"name":"my_tool","parameters":{}}}]}`)
	out, mapper, err := zenChatBody(raw, "big-pickle")
	if err != nil {
		t.Fatalf("zenChatBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools length = %d, want 3 (no placeholders needed)", len(tools))
	}
	names := make([]string, 0, 3)
	for _, item := range tools {
		entry, _ := item.(map[string]any)
		fn, _ := entry["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "read") || !strings.Contains(joined, "bash") {
		t.Fatalf("tools = %v, want lowercase read and bash", names)
	}
	if strings.Contains(joined, "Read") || strings.Contains(joined, "Bash") {
		t.Fatalf("tools = %v, client spelling was not rewritten", names)
	}
	if !mapper.declared("read") || !mapper.declared("bash") {
		t.Fatal("mapper did not record the client declarations")
	}
	if got := mapper.clientNameFor("read"); got != "Read" {
		t.Fatalf("clientNameFor(read) = %q, want Read", got)
	}
}

// TestToolGateRewritesHistory verifies tool names inside message history and
// tool_choice are translated too.
func TestToolGateRewritesHistory(t *testing.T) {
	raw := []byte(`{"model":"big-pickle","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}],"tool_choice":{"type":"function","function":{"name":"Bash"}},"tools":[{"type":"function","function":{"name":"Read"}},{"type":"function","function":{"name":"Bash"}}]}`)
	out, _, err := zenChatBody(raw, "big-pickle")
	if err != nil {
		t.Fatalf("zenChatBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("history tool name = %v, want read", fn["name"])
	}
	choice, _ := obj["tool_choice"].(map[string]any)
	cfn, _ := choice["function"].(map[string]any)
	if cfn["name"] != "bash" {
		t.Errorf("tool_choice name = %v, want bash", cfn["name"])
	}
}

// TestDownstreamDropsPlaceholderCalls verifies that a call to an injected
// placeholder is removed together with its argument-only continuation, and
// that finish_reason is rewritten when nothing survives.
func TestDownstreamDropsPlaceholderCalls(t *testing.T) {
	mapper := newToolMapper(nil) // client declared nothing
	filter := newDownstreamToolFilter(mapper)

	first := map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    float64(0),
			"id":       "call_1",
			"type":     "function",
			"function": map[string]any{"name": "read", "arguments": ""},
		}}},
		"finish_reason": nil,
	}}}
	filter.filterChunk(first)
	delta := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, present := delta["tool_calls"]; present {
		t.Fatalf("placeholder tool call was not dropped: %v", delta)
	}

	// Argument-only continuation for the dropped index must vanish too.
	second := map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    float64(0),
			"function": map[string]any{"arguments": "{\"path\":\"x\"}"},
		}}},
		"finish_reason": "tool_calls",
	}}}
	filter.filterChunk(second)
	choice := second["choices"].([]any)[0].(map[string]any)
	if _, present := choice["delta"].(map[string]any)["tool_calls"]; present {
		t.Fatal("argument-only continuation was not dropped")
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

// TestDownstreamKeepsClientTools verifies a real client tool survives with its
// own spelling restored.
func TestDownstreamKeepsClientTools(t *testing.T) {
	mapper := newToolMapper([]any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "Read"},
	}})
	filter := newDownstreamToolFilter(mapper)
	chunk := map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    float64(0),
			"id":       "call_9",
			"function": map[string]any{"name": "read", "arguments": "{}"},
		}}},
		"finish_reason": "tool_calls",
	}}}
	filter.filterChunk(chunk)
	choice := chunk["choices"].([]any)[0].(map[string]any)
	calls, _ := choice["delta"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("client tool call was dropped: %v", choice["delta"])
	}
	fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Read" {
		t.Fatalf("tool name = %v, want Read (client spelling)", fn["name"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

// TestCleanChunkStripsNulls verifies the upstream's explicit nulls are removed
// and an empty delta without finish_reason is dropped entirely.
func TestCleanChunkStripsNulls(t *testing.T) {
	withNulls := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null},"finish_reason":null}]}`
	got := cleanChunkJSON(withNulls, nil)
	if strings.Contains(got, "reasoning_content") || strings.Contains(got, "tool_calls") {
		t.Fatalf("null noise fields survived cleaning: %s", got)
	}
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Fatalf("useful content was lost: %s", got)
	}
	// finish_reason stays null on a non-final chunk: that is the OpenAI shape
	// clients expect, so only the delta is scrubbed.
	if !strings.Contains(got, `"finish_reason":null`) {
		t.Fatalf("finish_reason was rewritten: %s", got)
	}
	empty := `{"choices":[{"index":0,"delta":{"content":"","reasoning_content":null},"finish_reason":null}]}`
	if got := cleanChunkJSON(empty, nil); got != "" {
		t.Fatalf("empty delta was not dropped: %q", got)
	}
}

// TestAggregateCompletion folds a real upstream SSE transcript (captured from
// opencode.ai/zen/v1/chat/completions for big-pickle) into one completion.
func TestAggregateCompletion(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"ef172e66","object":"chat.completion.chunk","created":1792078770,"model":"big-pickle","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_content":null,"tool_calls":null}}]}`,
		`data: {"id":"ef172e66","object":"chat.completion.chunk","created":1792078770,"model":"big-pickle","choices":[{"index":0,"finish_reason":null,"delta":{"content":null,"reasoning_content":"The user wants \"say"}}]}`,
		`data: {"id":"ef172e66","object":"chat.completion.chunk","created":1792078770,"model":"big-pickle","choices":[{"index":0,"finish_reason":null,"delta":{"content":"Hello","reasoning_content":null}}]}`,
		`data: {"id":"ef172e66","object":"chat.completion.chunk","created":1792078770,"model":"big-pickle","choices":[{"index":0,"finish_reason":"stop","delta":{"content":" there friend"}}]}`,
		`data: {"id":"ef172e66","object":"chat.completion.chunk","created":1792078770,"model":"big-pickle","choices":[],"usage":{"prompt_tokens":119,"completion_tokens":19,"total_tokens":138,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":14}}}`,
		`data: [DONE]`,
	}, "\n")

	body, err := aggregateCompletion(strings.NewReader(sse), "big-pickle", newToolMapper(nil))
	if err != nil {
		t.Fatalf("aggregateCompletion: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", out["object"])
	}
	if out["model"] != "big-pickle" {
		t.Errorf("model = %v, want big-pickle", out["model"])
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello there friend" {
		t.Errorf("content = %v, want %q", msg["content"], "Hello there friend")
	}
	if msg["reasoning_content"] != `The user wants "say` {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(119) || usage["completion_tokens"] != float64(19) || usage["total_tokens"] != float64(138) {
		t.Errorf("usage = %v", usage)
	}
	if _, ok := usage["completion_tokens_details"]; !ok {
		t.Errorf("reasoning token details were dropped: %v", usage)
	}
}

// TestAggregateCompletionToolCalls verifies streamed tool-call fragments are
// reassembled, with placeholder calls dropped and client names restored.
func TestAggregateCompletionToolCalls(t *testing.T) {
	mapper := newToolMapper([]any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "Bash"},
	}})
	sse := strings.Join([]string{
		`data: {"id":"x","model":"big-pickle","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"id":"x","model":"big-pickle","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`data: {"id":"x","model":"big-pickle","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
		`data: [DONE]`,
	}, "\n")
	body, err := aggregateCompletion(strings.NewReader(sse), "big-pickle", mapper)
	if err != nil {
		t.Fatalf("aggregateCompletion: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one call", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Errorf("tool name = %v, want Bash", fn["name"])
	}
	if fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %v, want the reassembled JSON", fn["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

// TestModelRoutingScope verifies the router claims exactly the Zen models.
func TestModelRoutingScope(t *testing.T) {
	for _, id := range []string{"big-pickle", "opencodehub/big-pickle", "opencodehub-big-pickle", "deepseek-v4.1-flash-free"} {
		if !isZenModel(id) {
			t.Errorf("isZenModel(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "gpt-5.5", "workbuddy-bridge-deepseek-v4.1-flash", "claude-opus-5"} {
		if isZenModel(id) {
			t.Errorf("isZenModel(%q) = true, want false", id)
		}
	}
}

// TestStaticModelsNeverFail verifies registration always yields a model list,
// even with the proxy down (the refresh simply fails in the background).
func TestStaticModelsNeverFail(t *testing.T) {
	raw, err := handleModelStatic(nil)
	if err != nil {
		t.Fatalf("handleModelStatic returned an error: %v", err)
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
	if !env.OK {
		t.Fatalf("static response not ok: %s", raw)
	}
	if env.Result.Provider != providerName {
		t.Errorf("provider = %q, want %q", env.Result.Provider, providerName)
	}
	if len(env.Result.Models) < len(zenFreeModels) {
		t.Errorf("published %d models, want at least %d", len(env.Result.Models), len(zenFreeModels))
	}
}
