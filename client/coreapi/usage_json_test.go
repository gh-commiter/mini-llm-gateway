package coreapi

import (
	"encoding/json"
	"testing"

	"minigw/core"
)

// The report rows only gain fields; the durations are in milliseconds and unknown ones are counted, not added as zero.
func TestUsageJSONFailedTimeFields(t *testing.T) {
	b, err := json.Marshal(UsageJSON(core.UsageReport{Rows: []core.UsageRow{{Day: "2026-11-15", Provider: "codex", Model: "m", Events: 40, Failed: 33,
		ErrorEvents: 31, CancelledEvents: 1, InterruptedEvents: 1, DurationMs: 17400000, FailedDurationMs: 15000000, UnknownDuration: 3}}}))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Days []map[string]any }
	if err := json.Unmarshal(b, &got); err != nil || len(got.Days) != 1 {
		t.Fatal(string(b), err)
	}
	for k, want := range map[string]float64{"errorEvents": 31, "cancelledEvents": 1, "interruptedEvents": 1, "durationMs": 17400000, "failedDurationMs": 15000000,
		"unknownDurationEvents": 3, "events": 40, "failedEvents": 33} {
		if got.Days[0][k] != want {
			t.Errorf("%s = %v, want %v", k, got.Days[0][k], want)
		}
	}
}
