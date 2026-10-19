// executor.go — ProviderExecutor handlers for the Zen wire.
//
// Execute validates and aggregates one chat completion: the upstream rejects
// non-streaming requests outright, so the non-stream path runs the same SSE
// and folds it into a chat.completion object.
//
// ExecuteStream forwards the upstream OpenAI-shaped SSE. Each data event is
// cleaned (empty shells removed, tool calls translated back into the client's
// spelling) and emitted either synchronously as a chunk list or, when the host
// supplied a stream id, asynchronously via host.stream.emit/close.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// payloadOf returns the request body the executor should send upstream.
func payloadOf(req pluginapi.ExecutorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

// handleExecExecute serves a non-streaming client request by running the
// streaming upstream call and folding the SSE back into one completion.
func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("parse execute request: %w", err)
	}
	upstreamModel := resolveModel(req)
	body, mapper, err := zenChatBody(payloadOf(req), upstreamModel)
	if err != nil {
		return nil, err
	}
	resp, err := zenChatStream(context.Background(), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncateBody(rawErr))
	}
	completion, err := aggregateCompletion(resp.Body, upstreamModel, mapper)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion}), nil
}

// handleExecStream serves a streaming client request.
func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("parse stream request: %w", err)
	}
	upstreamModel := resolveModel(req.ExecutorRequest)
	body, mapper, err := zenChatBody(payloadOf(req.ExecutorRequest), upstreamModel)
	if err != nil {
		return nil, err
	}
	upstream, err := zenChatStream(context.Background(), body)
	if err != nil {
		return nil, err
	}
	if upstream.StatusCode != http.StatusOK {
		rawErr, _ := io.ReadAll(io.LimitReader(upstream.Body, errorBodyLimit))
		upstream.Body.Close()
		return nil, fmt.Errorf("upstream http %d: %s", upstream.StatusCode, truncateBody(rawErr))
	}

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)
	filter := newDownstreamToolFilter(mapper)

	// Async: the host created the stream and reads host.stream.emit pushes.
	// NOTE: the upstream body is closed INSIDE the pump goroutine, never by a
	// defer here — returning the envelope must not race the pump's reads (a
	// premature close surfaces to the client as "http2: response body closed"
	// or a truncated stream).
	if req.StreamID != "" {
		sid := req.StreamID
		go func() {
			errPump := pumpCleanedSSE(upstream.Body, sseFramed, filter, func(p []byte) error {
				return streamEmit(sid, p)
			})
			upstream.Body.Close()
			if errPump != nil {
				streamEmitError(sid, errPump.Error())
			}
			_ = streamClose(sid)
		}()
		return okEnvelope(streamResponse{Headers: headers}), nil
	}

	// Sync fallback: collect one cleaned chunk per data event.
	var chunks []pluginapi.ExecutorStreamChunk
	if _, errWalk := walkCleanedSSE(upstream.Body, sseFramed, filter, func(p string) error {
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(p)})
		return nil
	}); errWalk != nil {
		upstream.Body.Close()
		return nil, errWalk
	}
	upstream.Body.Close()
	return okEnvelope(streamResponse{Headers: headers, Chunks: chunks}), nil
}

// resolveModel picks the upstream model id: the host-resolved name (req.Model)
// wins, otherwise the id embedded in the body, in both cases stripped of any
// client-facing provider prefix.
func resolveModel(req pluginapi.ExecutorRequest) string {
	if target := normalizeUpstreamModel(req.Model); target != "" {
		return target
	}
	obj, ok := parseObjectBody(payloadOf(req))
	if !ok {
		return ""
	}
	name, _ := obj["model"].(string)
	return normalizeUpstreamModel(name)
}

// streamHeaders matches the other plugins' response headers: the host streams
// the client response as SSE, so Content-Type must be text/event-stream (never
// the upstream's own headers, which may carry length/framing mismatches).
func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// clientNeedsSSEFrame reports whether the host wants payloads pre-framed as
// "data: " lines. OpenAI chat/completions requests do not: the host frames
// bare JSON payloads itself and appends its own terminator.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// walkCleanedSSE reads an upstream SSE stream and emits one payload per data
// event: bare JSON (or a "data: " line when sseFramed), with keep-alive
// comments, empty shells and the trailing [DONE] dropped. It returns an error
// for a mid-stream read failure or a stream that carried no data event at all.
func walkCleanedSSE(r io.Reader, sseFramed bool, filter *downstreamToolFilter, emit func(string) error) (bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seenPayload := false
	for sc.Scan() {
		content, meaningful, _ := upstreamSSEFrame(sc.Text())
		seenPayload = seenPayload || meaningful
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content, filter)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := emit(cleaned); err != nil {
			return seenPayload, err
		}
	}
	if err := sc.Err(); err != nil {
		return seenPayload, err
	}
	if !seenPayload {
		return seenPayload, errors.New("upstream stream ended without a completion payload")
	}
	return seenPayload, nil
}

// pumpCleanedSSE is walkCleanedSSE for the asynchronous emit signature.
func pumpCleanedSSE(r io.Reader, sseFramed bool, filter *downstreamToolFilter, emit func([]byte) error) error {
	_, err := walkCleanedSSE(r, sseFramed, filter, func(p string) error { return emit([]byte(p)) })
	return err
}

// upstreamSSEFrame extracts the payload of one upstream SSE line: content is
// the JSON after "data:", meaningful reports whether the line was a data event
// (the empty-stream guard counts only those), and comment/blank/event lines
// carry no payload.
func upstreamSSEFrame(line string) (string, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return "", false, nil
	}
	if strings.HasPrefix(trimmed, "data:") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true, nil
	}
	return "", false, nil
}

// cleanChunkJSON normalises one upstream chunk:
//
//   - tool calls are translated back into the client's spelling, and calls to
//     an injected read/bash placeholder are dropped;
//   - empty shells are removed so strict clients do not mistake a terminal
//     chunk for a truncated tool call. The upstream emits explicit nulls
//     (reasoning_content:null, tool_calls:null) which many clients reject;
//   - a chunk whose delta is empty and which carries no finish_reason is
//     dropped entirely (returns "").
func cleanChunkJSON(s string, filter *downstreamToolFilter) string {
	if strings.HasPrefix(s, ":") {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if filter != nil {
		filter.filterChunk(obj)
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			for _, noise := range []string{"function_call", "extra_fields", "refusal", "reasoning_content", "tool_calls", "audio"} {
				v, present := delta[noise]
				if !present {
					continue
				}
				// A null or empty shell carries no information for the client.
				if v == nil || isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			if !deltaHasMeaning(delta) {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// deltaHasMeaning reports whether a delta still carries something a client can
// act on after the noise fields were removed. A delta that only ever held
// nulls/empty shells (the upstream's opening "role" frame keeps its role, but a
// content-only empty frame does not) is not worth forwarding.
func deltaHasMeaning(delta map[string]any) bool {
	for _, key := range []string{"role", "content", "reasoning_content", "tool_calls", "function_call"} {
		v, present := delta[key]
		if !present {
			continue
		}
		if key == "role" {
			if s, ok := v.(string); ok && s != "" {
				return true
			}
			continue
		}
		if !isEmptyValue(v) {
			return true
		}
	}
	return false
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case bool:
		return !t
	case float64:
		return t == 0
	}
	return false
}

// -----------------------------------------------------------------------------
// Non-stream aggregation
// -----------------------------------------------------------------------------

// toolCallAccum collects one streamed tool call: the upstream sends the name
// once and then streams the JSON arguments in fragments.
type toolCallAccum struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// aggregateCompletion folds an OpenAI-shaped SSE into a single chat.completion
// object for the non-stream executor path, applying the same tool-call
// translation as the streaming path.
func aggregateCompletion(r io.Reader, model string, mapper *toolMapper) ([]byte, error) {
	filter := newDownstreamToolFilter(mapper)

	var (
		id, respModel         string
		created               int64
		content, reason, role strings.Builder
		finish                string
		calls                 = map[int]*toolCallAccum{}
		order                 []int
		usage                 map[string]any
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(strings.TrimSpace(line), "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		if v, _ := obj["id"].(string); v != "" {
			id = v
		}
		if v, _ := obj["model"].(string); v != "" {
			respModel = v
		}
		if v, ok := obj["created"].(float64); ok && v > 0 {
			created = int64(v)
		}
		if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		// Apply the tool-call filter so dropped placeholder calls never reach
		// the aggregated message, and surviving names use the client's spelling.
		filter.filterChunk(obj)
		choices, _ := obj["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if fr, _ := choice["finish_reason"].(string); fr != "" {
				finish = fr
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			// The upstream repeats "role":"assistant" on every frame, so this
			// must keep the first non-empty value — appending turns nine
			// frames into "assistantassistantassistant…".
			if v, _ := delta["role"].(string); v != "" && role.Len() == 0 {
				role.WriteString(v)
			}
			if v, _ := delta["content"].(string); v != "" {
				content.WriteString(v)
			}
			if v, _ := delta["reasoning_content"].(string); v != "" {
				reason.WriteString(v)
			}
			collectToolCalls(delta["tool_calls"], calls, &order)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	if id == "" {
		id = "chatcmpl-" + randomSuffix()
	}
	if respModel == "" {
		respModel = model
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if role.Len() == 0 {
		role.WriteString("assistant")
	}
	if finish == "" {
		finish = "stop"
	}

	message := map[string]any{"role": role.String(), "content": content.String()}
	if reason.Len() > 0 {
		message["reasoning_content"] = reason.String()
	}
	if len(order) > 0 {
		sort.Ints(order)
		out := make([]any, 0, len(order))
		for _, idx := range order {
			acc := calls[idx]
			if acc == nil || acc.Name == "" {
				continue
			}
			out = append(out, map[string]any{
				"id":   acc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      acc.Name,
					"arguments": acc.Arguments.String(),
				},
			})
		}
		if len(out) > 0 {
			message["tool_calls"] = out
		} else if finish == "tool_calls" {
			// Every call targeted an injected placeholder: the client never
			// asked for tools, so report a plain stop.
			finish = "stop"
		}
	}

	body := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   respModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		body["usage"] = normalizeUsage(usage)
	}
	return json.Marshal(body)
}

// collectToolCalls merges one chunk's tool_calls array into the accumulator,
// preserving first-seen order.
func collectToolCalls(raw any, calls map[int]*toolCallAccum, order *[]int) {
	arr, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range arr {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		idx := toolCallIndex(call)
		acc := calls[idx]
		if acc == nil {
			acc = &toolCallAccum{Type: "function"}
			calls[idx] = acc
			*order = append(*order, idx)
		}
		if v, _ := call["id"].(string); v != "" {
			acc.ID = v
		}
		if v, _ := call["type"].(string); v != "" {
			acc.Type = v
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if v, _ := fn["name"].(string); v != "" {
			acc.Name = v
		}
		if v, _ := fn["arguments"].(string); v != "" {
			acc.Arguments.WriteString(v)
		}
	}
}

// normalizeUsage keeps the usage object OpenAI-shaped, deriving
// completion_tokens when the upstream only sent a total.
func normalizeUsage(usage map[string]any) map[string]any {
	prompt := intOf(usage["prompt_tokens"])
	total := intOf(usage["total_tokens"])
	completion := intOf(usage["completion_tokens"])
	if completion == 0 && total > 0 && total >= prompt {
		completion = total - prompt
	}
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	// Preserve the upstream's detail blocks (cached_tokens / reasoning_tokens)
	// so clients that display them keep working.
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		if v, ok := usage[key]; ok {
			out[key] = v
		}
	}
	return out
}

// intOf reads a JSON number as an int.
func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// randomSuffix makes a non-empty opaque completion id without crypto deps.
func randomSuffix() string {
	var b bytes.Buffer
	now := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		b.WriteByte("abcdef0123456789"[now&0xf])
		now >>= 4
	}
	return b.String()
}
