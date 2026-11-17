package main

// Usage events of the gateway (contracts section 7): the adapter side. It builds the event of a model request (who,
// which model, when the work was dispatched, how it ended); package coreapi records it without ever blocking the
// request (queue, spool, replay) and serves the usage routes.

import (
	"context"
	"strings"
	"time"

	"minigw/core"
)

// --- events of requests ---

// admitEvent (A5): the immutable admission context of a model request (user, device, parent, and the project of a
// nested call), built once when the request is admitted. Nothing is recorded yet: an event exists only once the work
// was dispatched (dispatched), so a local refusal never becomes one.
func (s *server) admitEvent(r *req, model string) {
	if s.api == nil || r.URL.Path != "/v1/messages" {
		return
	}
	e := core.UsageEvent{ID: "ev_" + randomToken()[:16], Start: time.Now(), Model: model, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", Outcome: core.OutcomeOpen}
	if d := r.conn.device; d != nil {
		e.DeviceID = d.ID
		e.UserID = s.api.UserOf(r.Context(), d.ID)
	}
	// Labels of the run (phase, activity, task): data for reports, never a reason to refuse. The gateway applies its own
	// limits (a modified client): a value that is not a safe token is dropped, the request goes on.
	e.Phase, e.Activity = core.SafeWordLabel(r.h("x-claude-mini-phase")), core.SafeWordLabel(r.h("x-claude-mini-activity"))
	e.Task, e.TaskSource = core.SafeTaskLabel(r.h("x-claude-mini-task")), r.h("x-claude-mini-task-source")
	if run := runOf(r); run != nil { // a nested call: the caller's event is the parent; its user and project are inherited
		e.ParentID, e.UserID = run.evID, run.evUser
		e.Phase, e.Activity, e.Task, e.TaskSource = run.phase, run.activity, run.task, run.taskSource
		if run.proj != nil {
			e.ProjectID = run.proj.project
		}
	}
	r.event = &e
}

// dispatched marks the moment execution or the upstream request really begins (a provider request, an agent process,
// a Foundry request) and records the event as begun, with its project (also an interrupted run keeps it).
func (s *server) dispatched(r *req) {
	e := r.event
	if e == nil || r.evDispatched {
		return
	}
	r.evDispatched, r.evStarted = true, time.Now()
	if r.project != nil {
		e.ProjectID = r.project.project
	}
	e.Provider = r.provider
	if agentOfModel(e.Model) != "" && e.UpstreamModel == "" {
		e.BillingMode = "subscription"
	}
	if r.provider == "foundry" {
		e.BillingMode = "local"
	}
	s.api.EventBegin(*e)
}

// endEvent completes the event of a request that was dispatched; any status then is an outcome (an upstream 4xx is an
// error event). A request that never reached dispatch has no event.
func (s *server) endEvent(r *req, status int, uw *usageWriter) {
	e := r.event
	if e == nil || !r.evDispatched {
		return
	}
	e.End = time.Now()
	e.DurationMs = new(e.End.Sub(r.evStarted).Milliseconds())
	e.Provider = r.provider
	if e.Provider == "" {
		if a := agentOfModel(e.Model); a != "" {
			e.Provider = a
		} else {
			e.Provider = "unknown"
		}
	}
	switch {
	case status == 499 || r.Context().Err() != nil:
		e.Outcome = core.OutcomeCancelled
	case status >= 400 || r.errKind != "" || uw != nil && uw.errSeen: // 429, 5xx, an upstream 4xx, a stream that broke or carried an error event after 200, an agent that failed
		e.Outcome = core.OutcomeError
		e.ErrorKind = errorKind(r, status, uw)
	default:
		e.Outcome = core.OutcomeOK
	}
	if uw != nil && (uw.in > 0 || uw.out > 0) {
		e.TokensIn, e.TokensOut = uw.in, uw.out
		e.TokenSource = "reported"
		if r.tokenSource != "" {
			e.TokenSource = r.tokenSource // estimated: Codex, Grok and Copilot give no counts, the gateway estimates the output
		}
		if e.TokensIn == 0 && e.TokenSource == "estimated" {
			e.TokensIn = -1
		}
	}
	if uw != nil && r.tokenSource != "estimated" {
		in, out := uw.metrics.input, uw.metrics.output
		if e.UpstreamModel == "" && in != nil && out != nil && *in == 0 && *out == 0 {
			in, out = nil, nil // an agent's message_start carries 0/0 before any count exists: an aborted run is unknown, not "reported 0/0"
		}
		// Nothing found by the structured parser (nesting, a usage object over 64 KB, usage elsewhere): the pattern counts set above stay.
		if in != nil || out != nil {
			e.TokensIn, e.TokensOut = -1, -1
			if in != nil {
				e.TokensIn = *in
			}
			if out != nil {
				e.TokensOut = *out
			}
			e.TokenSource = "reported"
		}
		// CLI subscription costs are not charges attributable to this gateway.
		if e.UpstreamModel != "" && uw.metrics.cost != nil {
			e.CostUsdNanos = uw.metrics.cost
			e.CostSource = "reported"
		}
	}
	if uw != nil {
		e.CacheReadTokens, e.CacheWriteTokens = uw.metrics.cacheRead, uw.metrics.cacheWrite
	}
	if uw != nil && uw.metrics.cacheUsed {
		e.UnpricedDimensions = true
	}
	s.api.EventFinish(*e)
}

// fail marks a request as failed although it answered with a success status (a stream that broke after 200, an agent
// that exited non-zero but gave text); the first reason stays. kind is one of core.ErrorKinds.
func (r *req) fail(kind string) {
	if r.errKind == "" {
		r.errKind = kind
	}
}

// errorKind: the bounded reason of an error event, never an error text.
func errorKind(r *req, status int, uw *usageWriter) string {
	switch {
	case r.errKind != "":
		return r.errKind
	case r.event.UpstreamStatus >= 400:
		return "http"
	case uw != nil && uw.errSeen:
		return "stream"
	case status == 408 || status == 504:
		return "timeout"
	}
	return "other"
}

// agentFailKind: the deadline decides first, because a killed run also gets a "failed (...)" message.
func agentFailKind(ctx context.Context, msg string) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "timeout"
	}
	return agentErrKind(msg)
}

// agentErrKind: the reason of an agent run's error (the messages of runAgentProcess and the Claude CLI path).
func agentErrKind(msg string) string {
	switch {
	case strings.Contains(msg, "stopped after"):
		return "timeout"
	case agentLimit(msg):
		return "limit"
	case strings.Contains(msg, " failed ("):
		return "exit"
	}
	return "other"
}

// usageErrSeen reports an SSE error event in a chunk of the stream.
func usageErrSeen(b []byte) bool { return strings.Contains(string(b), "event: error\n") }
