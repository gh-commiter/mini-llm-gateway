package main

// The quota routes of the admin API (settings and report). They stay on the gateway: the probes run CLIs and
// provider HTTP here. The rest of the core's admin routes are in package coreapi.

import (
	"context"
)

// quotaAdmin serves GET and PATCH /quota.
func (s *server) quotaAdmin(method string, seg []string, body []byte) (int, any) {
	fail := func(status int, msg string) (int, any) { return status, errorBody(status, msg) }
	if len(seg) != 1 {
		return fail(404, "Not found: "+method+" "+seg[0])
	}
	switch method {
	case "PATCH": // {"claudeProbe": bool}: the opt-in of the Claude probe; it runs nothing by itself (the next GET does)
		var b struct{ ClaudeProbe *bool }
		if decodeStrict(body, &b) != nil || b.ClaudeProbe == nil {
			return fail(400, "bad JSON body (claudeProbe)")
		}
		if err := s.update(func(c *serveConfig) error { c.ClaudeQuotaProbe = *b.ClaudeProbe; return nil }); err != nil {
			return fail(500, err.Error())
		}
	case "GET":
	default:
		return fail(404, "Not found: "+method+" "+seg[0])
	}
	return 200, map[string]any{"quota": s.quotaReportF(context.Background(), method == "GET"), "claudeProbe": s.conf().ClaudeQuotaProbe}
}
