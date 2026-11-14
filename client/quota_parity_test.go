package main

// Parity of the quota parsers with the Mac client (Sources/RouterCore/Quota.swift): client/testdata/quota-parsers.json holds the Codex
// rate-limit lines, the `claude -p /usage` output, the reset texts (named time zones), and the OpenRouter and LiteLLM answers with the values
// they must give. Asserted here and in Tests/RouterCoreTests/QuotaTests.swift. MGW_UPDATE_FIXTURES=1 rewrites the expected values from this
// implementation: review the diff before committing it.

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

type quotaCase struct {
	Name  string          `json:"name"`
	Kind  string          `json:"kind"` // codex, claude-usage, claude-reset, openrouter, litellm, stale
	Input string          `json:"input"`
	Now   string          `json:"now"`
	Want  json.RawMessage `json:"want,omitempty"` // the values (a list), a reset time (a string, or null), the stale flags, or {"error": "…"}
}

func runQuotaCase(t *testing.T, c *quotaCase) any {
	now, err := time.Parse(time.RFC3339, c.Now)
	if err != nil {
		t.Fatal(c.Name, err)
	}
	var vals []quotaValue
	switch c.Kind {
	case "codex":
		vals = parseCodexRateLimits([]byte(c.Input), now)
	case "claude-usage":
		vals, err = parseClaudeUsage([]byte(c.Input), now)
	case "openrouter":
		vals, err = parseOpenRouterCredits([]byte(c.Input), now)
	case "litellm":
		vals, err = parseLiteLLMKeyInfo([]byte(c.Input), now)
	case "claude-reset":
		if r := parseClaudeReset(c.Input, now); r != nil {
			return r.UTC().Format(time.RFC3339)
		}
		return nil
	case "stale":
		var in []quotaValue
		if err := json.Unmarshal([]byte(c.Input), &in); err != nil {
			t.Fatal(c.Name, err)
		}
		var flags []bool
		for _, v := range markStale(in, now) {
			flags = append(flags, v.Stale)
		}
		return flags
	default:
		t.Fatal("kind", c.Kind)
	}
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	b, _ := json.Marshal(vals)
	var out any
	json.Unmarshal(b, &out)
	if out == nil {
		out = []any{}
	}
	return out
}

func TestQuotaParityParsers(t *testing.T) {
	var fx struct {
		Cases []*quotaCase `json:"cases"`
	}
	readFixture(t, "quota-parsers.json", &fx)
	saved := time.Local
	time.Local = time.UTC // a reset text without a zone is read in the local zone; the fixture is in UTC
	defer func() { time.Local = saved }()
	if len(fx.Cases) < 60 {
		t.Fatalf("only %d cases", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := runQuotaCase(t, c)
			gb, _ := json.Marshal(got)
			if updateFixtures() {
				c.Want = gb
				return
			}
			var want any
			json.Unmarshal(c.Want, &want)
			var gotAny any
			json.Unmarshal(gb, &gotAny)
			if !reflect.DeepEqual(gotAny, want) {
				t.Fatalf("\n got %s\nwant %s", gb, c.Want)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "quota-parsers.json", fx)
	}
}
