package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

// normalizeForProvider: api.x.ai rejects a role "system" inside messages and a tool input_schema without "required"
// (Claude Code sends both). Applied only to that host, so OpenRouter and LiteLLM bodies stay as they are.
// Shared test cases with the Mac gateway: testdata/provider-normalize.json (Sources/RouterCore/ProviderBody.swift).
func normalizeForProvider(baseURL string, body map[string]json.RawMessage) {
	if u, err := url.Parse(baseURL); err != nil || strings.ToLower(u.Hostname()) != "api.x.ai" {
		return
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(body["tools"], &tools) == nil {
		changed := false
		for _, t := range tools {
			var schema map[string]json.RawMessage
			if json.Unmarshal(t["input_schema"], &schema) != nil || string(schema["type"]) != `"object"` {
				continue
			}
			if r := string(schema["required"]); r == "" || r == "null" {
				schema["required"] = json.RawMessage("[]")
				t["input_schema"], _ = json.Marshal(schema)
				changed = true
			}
		}
		if changed {
			body["tools"], _ = json.Marshal(tools)
		}
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(body["messages"], &msgs) != nil {
		return
	}
	hasSystem := false
	for _, m := range msgs {
		hasSystem = hasSystem || string(m["role"]) == `"system"`
	}
	if !hasSystem {
		return
	}
	var out []map[string]json.RawMessage
	fresh := false // the last message in out holds a converted system note
	for _, m := range msgs {
		blocks := contentBlocks(m["content"])
		note := string(m["role"]) == `"system"`
		if note {
			var texts []string
			for _, b := range blocks {
				var tb struct{ Text string }
				json.Unmarshal(b, &tb)
				texts = append(texts, tb.Text)
			}
			b, _ := json.Marshal(map[string]string{"type": "text", "text": "<system-reminder>\n" + strings.Join(texts, "\n") + "\n</system-reminder>"})
			blocks = []json.RawMessage{b}
			m = map[string]json.RawMessage{"role": json.RawMessage(`"user"`)}
		}
		if last := len(out) - 1; last >= 0 && string(m["role"]) == `"user"` && string(out[last]["role"]) == `"user"` && (note || fresh) {
			// tool_result blocks stay first: the API wants them right after the tool_use
			all := append(contentBlocks(out[last]["content"]), blocks...)
			var head, rest []json.RawMessage
			for _, b := range all {
				var tb struct{ Type string }
				json.Unmarshal(b, &tb)
				if tb.Type == "tool_result" {
					head = append(head, b)
				} else {
					rest = append(rest, b)
				}
			}
			out[last]["content"], _ = json.Marshal(append(head, rest...))
		} else {
			if note {
				m["content"], _ = json.Marshal(blocks)
			}
			out = append(out, m)
		}
		fresh = note
	}
	body["messages"], _ = json.Marshal(out)
}

// contentBlocks: message content as a block list (a string becomes one text block).
func contentBlocks(c json.RawMessage) []json.RawMessage {
	var s string
	if json.Unmarshal(c, &s) == nil {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return []json.RawMessage{b}
	}
	var blocks []json.RawMessage
	json.Unmarshal(c, &blocks)
	return blocks
}
