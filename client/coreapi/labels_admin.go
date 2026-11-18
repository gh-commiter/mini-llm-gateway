package coreapi

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"minigw/core"
)

// adminLabels serves GET|PUT /usage/labels/settings and GET /usage/labels/agreement: the owner's settings of the model-filled
// labels and the counters of the shadow comparison. Admin channel only (adminDo is reached by the owner alone); a member route
// never sees them. The settings come as a one-row list (the panel edits a row).
func (a *API) adminLabels(ctx context.Context, st *core.Store, method, name string, q url.Values, body []byte, failErr func(error) (int, any, bool)) (int, any, bool) {
	bad := func(msg string) (int, any, bool) { return 400, errorBody(400, msg), true }
	row := func(l core.LabelSettings) any {
		return map[string]any{"settings": []any{struct {
			ID string `json:"id"`
			core.LabelSettings
		}{"labels", l}}}
	}
	switch method + " " + name {
	case "GET settings":
		l, err := st.LabelSettings(ctx)
		if err != nil {
			return failErr(err)
		}
		return 200, row(l), true
	case "PUT settings":
		a.labelsMu.Lock()
		defer a.labelsMu.Unlock()
		var p core.LabelSettingsPatch
		if decodeStrict(body, &p) != nil {
			return bad("settings: enabled, backend, threshold, maxPerHour, shadow, shadowRate, activities")
		}
		if msg := a.labelBackendRefusal(p.Backend); msg != "" {
			return bad(msg)
		}
		l, err := st.SetLabelSettings(ctx, p)
		if err != nil {
			return failErr(err)
		}
		if a.o.LabelsChanged != nil {
			a.o.LabelsChanged(l)
		}
		return 200, row(l), true
	case "GET agreement":
		days := 0
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return bad("days: a number")
			}
			days = n
		}
		rows, err := st.Agreement(ctx, days)
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"agreement": rows}, true
	}
	return 404, errorBody(404, "Not found: "+method+" usage/labels/"+name), true
}

// labelBackendRefusal: why the backend chosen is not available on this host ("" = fine, or no restriction).
func (a *API) labelBackendRefusal(backend *string) string {
	if backend == nil || *backend == "" || a.o.LabelBackends == nil {
		return ""
	}
	for _, ok := range a.o.LabelBackends {
		if *backend == ok || strings.HasSuffix(ok, ":") && strings.HasPrefix(*backend, ok) {
			return ""
		}
	}
	if len(a.o.LabelBackends) == 0 {
		return "backend: this system has no on-device model for labels"
	}
	return "backend: not available on this system; choose " + strings.Join(a.o.LabelBackends, " or ")
}
