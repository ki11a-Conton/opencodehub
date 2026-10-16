// tools.go — the read/bash tool gate.
//
// Ported from src/converters/toolMapping.ts in ki11a-Conton/OpenCodeProxyHub.
// The Zen free tier only accepts requests that declare the two tools the real
// OpenCode CLI always ships:
//
//	read  and  bash      (lowercase, case sensitive)
//
// Downstream clients spell them Read/Bash. So this file
//
//  1. renames the client's read/bash entries to the lowercase upstream names,
//  2. injects a never-callable placeholder for whichever one the client did not
//     declare (the upstream requires both to be present),
//  3. rewrites tool names in message history and tool_choice,
//  4. on the way back, restores the client's spelling and DROPS any tool call
//     that targets an injected placeholder.
//
// Every other tool name passes through untouched in both directions.
package main

import (
	"encoding/json"
	"strings"
)

const (
	// upstreamReadTool / upstreamBashTool are the exact names the gate wants.
	upstreamReadTool = "read"
	upstreamBashTool = "bash"

	placeholderReadDesc = "Placeholder for OpenCode's built-in file reader. It is NOT available in this session: must never be called, and any such call is discarded."
	placeholderBashDesc = "Placeholder for OpenCode's built-in shell. It is NOT available in this session: must never be called, and any such call is discarded."
)

// toolMapper carries the per-request tool translation state: which of the two
// gated tools the client actually declared, and under which spelling.
type toolMapper struct {
	// clientName maps the upstream name ("read"/"bash") to the client's own
	// spelling when the client declared it; the key is absent when it did not.
	clientName map[string]string
	// upstreamName is the reverse map used when rewriting request history.
	upstreamName map[string]string
}

// newToolMapper inspects the client's tools array and records which gated
// tools it declared. A nil or malformed array means "declared nothing".
func newToolMapper(tools any) *toolMapper {
	m := &toolMapper{
		clientName:   map[string]string{},
		upstreamName: map[string]string{},
	}
	arr, ok := tools.([]any)
	if !ok {
		return m
	}
	for _, item := range arr {
		name := toolFunctionName(item)
		if name == "" {
			continue
		}
		switch strings.ToLower(name) {
		case upstreamReadTool:
			if _, seen := m.clientName[upstreamReadTool]; !seen {
				m.clientName[upstreamReadTool] = name
				m.upstreamName[name] = upstreamReadTool
			}
		case upstreamBashTool:
			if _, seen := m.clientName[upstreamBashTool]; !seen {
				m.clientName[upstreamBashTool] = name
				m.upstreamName[name] = upstreamBashTool
			}
		}
	}
	return m
}

// toolFunctionName extracts function.name from one tools[] entry.
func toolFunctionName(item any) string {
	entry, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := entry["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return strings.TrimSpace(name)
}

// declared reports whether the client declared the given gated tool itself.
func (m *toolMapper) declared(upstream string) bool {
	_, ok := m.clientName[upstream]
	return ok
}

// upstreamToolList rewrites a client tools array for the upstream and appends
// the placeholders the upstream gate requires.
func (m *toolMapper) upstreamToolList(tools any) []any {
	out := make([]any, 0, 4)
	if arr, ok := tools.([]any); ok {
		for _, item := range arr {
			entry, ok := item.(map[string]any)
			if !ok {
				out = append(out, item)
				continue
			}
			name := toolFunctionName(item)
			if mapped, ok := m.upstreamName[name]; ok {
				entry = cloneObject(entry)
				fn, _ := entry["function"].(map[string]any)
				fn = cloneObject(fn)
				fn["name"] = mapped
				entry["function"] = fn
			}
			out = append(out, entry)
		}
	}
	for _, gated := range []struct {
		name string
		desc string
	}{
		{upstreamReadTool, placeholderReadDesc},
		{upstreamBashTool, placeholderBashDesc},
	} {
		if m.declared(gated.name) {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        gated.name,
				"description": gated.desc,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return out
}

// upstreamMessages rewrites historical tool_calls names in place (on copies).
func (m *toolMapper) upstreamMessages(messages []any) []any {
	if len(m.upstreamName) == 0 {
		return messages
	}
	out := make([]any, 0, len(messages))
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			out = append(out, item)
			continue
		}
		msg = cloneObject(msg)
		rewritten := make([]any, 0, len(calls))
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				rewritten = append(rewritten, c)
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				rewritten = append(rewritten, c)
				continue
			}
			name, _ := fn["name"].(string)
			mapped, ok := m.upstreamName[name]
			if !ok {
				rewritten = append(rewritten, c)
				continue
			}
			call = cloneObject(call)
			fn = cloneObject(fn)
			fn["name"] = mapped
			call["function"] = fn
			rewritten = append(rewritten, call)
		}
		msg["tool_calls"] = rewritten
		out = append(out, msg)
	}
	return out
}

// upstreamToolChoice maps a named tool_choice onto the upstream spelling.
// "auto"/"none"/"required" and object forms for other tools pass through.
func (m *toolMapper) upstreamToolChoice(choice any) any {
	obj, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	fn, ok := obj["function"].(map[string]any)
	if !ok {
		return choice
	}
	name, _ := fn["name"].(string)
	mapped, ok := m.upstreamName[name]
	if !ok {
		return choice
	}
	out := cloneObject(obj)
	fn = cloneObject(fn)
	fn["name"] = mapped
	out["function"] = fn
	return out
}

// -----------------------------------------------------------------------------
// Downstream (response) side
// -----------------------------------------------------------------------------

// downstreamToolFilter rewrites tool calls in upstream chunks back into the
// client's spelling and removes calls to injected placeholders. It keeps the
// per-stream state needed to drop the argument-only chunks that follow a
// dropped call.
type downstreamToolFilter struct {
	mapper *toolMapper
	// dropped tracks tool_call indexes that were removed, so later chunks
	// carrying only arguments for that index are removed too.
	dropped map[int]bool
	// sawCall reports whether any tool call survived the filter.
	sawCall bool
}

func newDownstreamToolFilter(m *toolMapper) *downstreamToolFilter {
	if m == nil {
		m = newToolMapper(nil)
	}
	return &downstreamToolFilter{mapper: m, dropped: map[int]bool{}}
}

// clientName returns the spelling the client used for an upstream gated tool.
func (m *toolMapper) clientNameFor(upstream string) string {
	if name, ok := m.clientName[upstream]; ok {
		return name
	}
	return upstream
}

// filterChunk rewrites one upstream chunk object in place and reports whether
// the chunk still carries anything worth forwarding.
func (f *downstreamToolFilter) filterChunk(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return true
	}
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if ok {
			f.filterDelta(delta)
		}
		if fr, _ := choice["finish_reason"].(string); fr == "tool_calls" && !f.sawCall {
			// Every call in this response targeted a placeholder: the client
			// never asked for tools, so report a plain stop.
			choice["finish_reason"] = "stop"
		}
	}
	return true
}

// filterDelta rewrites delta.tool_calls: names are restored to the client's
// spelling, placeholder calls are dropped together with their argument chunks.
func (f *downstreamToolFilter) filterDelta(delta map[string]any) {
	calls, ok := delta["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return
	}
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			out = append(out, c)
			continue
		}
		idx := toolCallIndex(call)
		fn, hasFn := call["function"].(map[string]any)
		name := ""
		if hasFn {
			name, _ = fn["name"].(string)
		}
		if name != "" {
			switch strings.ToLower(name) {
			case upstreamReadTool, upstreamBashTool:
				gated := strings.ToLower(name)
				if !f.mapper.declared(gated) {
					f.dropped[idx] = true
					continue
				}
				call = cloneObject(call)
				fn = cloneObject(fn)
				fn["name"] = f.mapper.clientNameFor(gated)
				call["function"] = fn
			default:
				// A real client tool: untouched.
			}
			f.sawCall = true
		} else if f.dropped[idx] {
			// Argument-only continuation of a dropped call.
			continue
		}
		out = append(out, call)
	}
	if len(out) == 0 {
		delete(delta, "tool_calls")
		return
	}
	delta["tool_calls"] = out
}

// toolCallIndex reads the tool_call index, defaulting to 0 (the upstream
// always sends one, but a missing index must not collapse two calls).
func toolCallIndex(call map[string]any) int {
	switch v := call["index"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// cloneObject shallow-copies a JSON object so edits never mutate the decoded
// original (which other code may still reference).
func cloneObject(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
