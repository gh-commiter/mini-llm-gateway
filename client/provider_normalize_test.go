package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeForProvider(t *testing.T) {
	var f struct {
		Hosts []struct {
			URL     string
			Applies bool
		}
		Cases []struct {
			Name            string
			Input, Expected map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(srcFile(t, "testdata/provider-normalize.json"), &f); err != nil || len(f.Cases) == 0 {
		t.Fatal(err)
	}
	canon := func(b map[string]json.RawMessage) any {
		raw, _ := json.Marshal(b)
		var v any
		json.Unmarshal(raw, &v)
		return v
	}
	cp := func(b map[string]json.RawMessage) map[string]json.RawMessage {
		o := map[string]json.RawMessage{}
		for k, v := range b {
			o[k] = v
		}
		return o
	}
	for _, c := range f.Cases {
		got := cp(c.Input)
		normalizeForProvider("https://api.x.ai", got)
		if !reflect.DeepEqual(canon(got), canon(c.Expected)) {
			g, _ := json.Marshal(got)
			t.Errorf("%s:\n got %s", c.Name, g)
		}
		again := cp(got)
		normalizeForProvider("https://api.x.ai", again)
		if !reflect.DeepEqual(canon(again), canon(got)) {
			t.Errorf("%s: not idempotent", c.Name)
		}
		other := cp(c.Input)
		normalizeForProvider("https://openrouter.ai/api", other)
		if !reflect.DeepEqual(other, c.Input) {
			t.Errorf("%s: changed for another provider", c.Name)
		}
	}
	for _, h := range f.Hosts {
		in := map[string]json.RawMessage{"tools": json.RawMessage(`[{"input_schema":{"type":"object"}}]`)}
		normalizeForProvider(h.URL, in)
		if changed := string(in["tools"]) != `[{"input_schema":{"type":"object"}}]`; changed != h.Applies {
			t.Errorf("%s: applies=%v, want %v", h.URL, changed, h.Applies)
		}
	}
}
