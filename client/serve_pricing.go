package main

import (
	"encoding/json"
	"minigw/core"
	"strings"
	"time"
)

// Price lists use USD per token, not USD per million tokens.
func priceRates(m map[string]any, openRouter bool) *core.PriceRates {
	in, out := "input_cost_per_token", "output_cost_per_token"
	if openRouter {
		in, out = "prompt", "completion"
	}
	decimal := func(v any) string {
		switch n := v.(type) {
		case string:
			return n
		case json.Number:
			return string(n)
		}
		return ""
	}
	p := core.PriceRates{Unit: "USD/token", Input: decimal(m[in]), Output: decimal(m[out])}
	if !p.Valid() {
		return nil
	}
	for k, v := range m {
		if k == in || k == out || strings.Contains(k, "cache") {
			continue
		}
		if openRouter || (strings.Contains(k, "cost") || strings.Contains(k, "tier") || strings.Contains(k, "multiplier")) {
			if v == nil {
				continue
			}
			if n := rateZero(decimal(v)); !n {
				return nil
			}
		}
	}
	return &p
}

func rateZero(s string) bool {
	if !(core.PriceRates{Unit: "USD/token", Input: s, Output: "0"}).Valid() {
		return false
	}
	mantissa := strings.FieldsFunc(s, func(r rune) bool { return r == 'e' || r == 'E' })[0]
	return strings.Trim(mantissa, "0.") == ""
}
func snapshot(provider, model, source string, rates *core.PriceRates) *core.PriceSnapshot {
	if rates == nil {
		return nil
	}
	at := time.Now().UTC()
	return &core.PriceSnapshot{ID: "ps_" + randomToken()[:24], Provider: provider, UpstreamModel: model, FetchedAt: at, EffectiveDay: at.Format("2006-01-02"), Source: source, Rates: *rates}
}

// Only complete input and output rates of the owner's own proxy count; a deployment without them has no price.
func deploymentPrices(data []any) map[string]*core.PriceRates {
	counts := map[string]int{}
	out := map[string]*core.PriceRates{}
	for _, it := range data {
		m, _ := it.(map[string]any)
		name, _ := m["model_name"].(string)
		counts[name]++
	}
	for _, it := range data {
		m, _ := it.(map[string]any)
		name, _ := m["model_name"].(string)
		if name == "" || counts[name] != 1 {
			continue
		}
		mi, _ := m["model_info"].(map[string]any)
		params, _ := m["litellm_params"].(map[string]any)
		local := map[string]any{}
		for _, overrides := range []map[string]any{mi, params} {
			for k, v := range overrides {
				if v != nil {
					local[k] = v
				}
			}
		}
		merged := map[string]any{}
		for k, v := range local {
			merged[k] = v
		}
		out[name] = priceRates(merged, false)
	}
	return out
}
