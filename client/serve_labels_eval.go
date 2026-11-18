package main

// minigw gw labels eval: the owner's local check of the label model against hand-labelled examples (docs/plans/brief-decide-labels.md
// B4). The examples arrive in the request body of POST /usage/labels/eval (an admin route of the gateway, like quota: never a
// coreapi route, never on the remote settings listener), each goes through the SAME decision the label worker makes (decideLabels,
// meteredBackend, the foreground coordination of labelSample), and only numbers come back. Nothing is stored: no label, no agreement
// counter, no text. The one record is the worker's own accounting rule: ONE internal usage event for the whole run (no user,
// device "gateway", sources "internal", tokens estimated), so the on-device work is visible in the reports as usage-labels.
// The example text lives in memory for the run; it is never logged, never put in an error, never returned (the client keeps its own
// copy and shows it only with --show-errors).

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"minigw/core"
	"minigw/decide"
)

const (
	labelEvalMaxExamples = 500
	labelEvalBudget      = 30 * time.Minute // the whole run; an example not reached counts as not run
	labelEvalAttempts    = 3                // an example whose sample a user's request ended is tried again (the user waits for nothing)
	labelEvalMaxWrong    = 100              // the examples listed for --show-errors
	labelEvalMaxConfused = 5                // the most frequent mistakes per kind
)

// labelMeter counts what the samples of one decision used; preempted: a user's request ended a sample.
type labelMeter struct {
	in, out   int64
	preempted bool
}

// meteredBackend: one sample at a time, only while no user request runs on the backend (labelSample), counted for the internal event.
// A user's request ending the sample cancels the decision (cancel) and is reported by meter.preempted.
func (s *server) meteredBackend(backend decide.Backend, m *labelMeter, cancel context.CancelFunc) decide.Backend {
	return func(c context.Context, system, user string) (string, error) {
		sample, release, err := s.foundry.labelSample(c)
		if err != nil {
			return "", err
		}
		defer release()
		m.in += int64((len(system) + len(user) + 3) / 4)
		reply, err := backend(sample, system, user)
		if context.Cause(sample) == errLabelForeground {
			m.preempted = true
			cancel()
			return "", decide.ErrBackend
		}
		m.out += int64((len(reply) + 3) / 4)
		return reply, err
	}
}

// labelBackend: the completion of the chosen model (tests may put their own into labels.backend).
func (s *server) labelBackend(model string) decide.Backend {
	if b := s.labels.backend; b != nil {
		return b
	}
	return func(c context.Context, system, user string) (string, error) {
		return s.foundryComplete(c, model, system, user)
	}
}

// decideLabels is THE decision, shared by the worker and eval: the phase, then the activity, each by three samples. A kind that is
// not wanted is not asked (its result stays empty).
func decideLabels(ctx context.Context, backend decide.Backend, activities []string, input string, threshold float64, phase, activity bool) (res [2]decide.Result, err error) {
	if phase {
		res[0], err = decide.Decide(ctx, backend, decide.Request{Question: "Which phase describes this work?", Candidates: labelCandidates(core.PhaseWords), Input: input}, threshold)
	}
	if err == nil && activity {
		res[1], err = decide.Decide(ctx, backend, decide.Request{Question: "Which activity describes this work?", Candidates: labelCandidates(activities), Input: input}, threshold)
	}
	return
}

// The wire format. Fields are in alphabetical order: the Swift client writes sorted keys and the parity fixtures compare the text.
type labelEvalExample struct {
	Activity string `json:"activity"`
	Phase    string `json:"phase"`
	Text     string `json:"text"`
}
type labelEvalRequest struct {
	Examples []labelEvalExample `json:"examples"`
}
type labelEvalCell struct {
	Correct int `json:"correct"` // decided and equal to the hand label
	Decided int `json:"decided"` // the model chose a label (coverage = decided / scored)
}
type labelEvalKind struct {
	One    labelEvalCell `json:"t10"`    // all three samples agree
	Scored int           `json:"scored"` // examples run that carry a hand label of this kind
	Two    labelEvalCell `json:"t067"`   // two of three agree (3 of 3 included)
}
type labelEvalConfusion struct {
	Expected string `json:"expected"`
	Got      string `json:"got"`
	N        int    `json:"n"`
}
type labelEvalWrong struct {
	Expected string `json:"expected"`
	Got      string `json:"got"` // "" = the model did not decide
	Kind     string `json:"kind"`
	Line     int    `json:"line"` // the example's place in the request (1 = first)
}
type labelEvalResult struct {
	Activity labelEvalKind                   `json:"activity"`
	Backend  string                          `json:"backend"`
	Examples int                             `json:"examples"`
	Mistakes map[string][]labelEvalConfusion `json:"mistakes"` // at two of three, per kind, most frequent first
	NotRun   int                             `json:"notRun"`   // failed, ended by a user's request three times, or after the time budget
	Phase    labelEvalKind                   `json:"phase"`
	Run      int                             `json:"run"`
	Wrong    []labelEvalWrong                `json:"wrong"` // at two of three; capped
}

func (s *server) labelsEval(ctx context.Context, body []byte) (int, any) {
	if s.offline || s.core == nil {
		return 503, errorBody(503, "The gateway is not running (or its project database is not available): label evaluation runs on the gateway, where the model is")
	}
	var req labelEvalRequest
	if err := decodeStrict(body, &req); err != nil {
		return 400, errorBody(400, "examples: a list of {text, phase, activity}")
	}
	if len(req.Examples) == 0 || len(req.Examples) > labelEvalMaxExamples {
		return 400, errorBody(400, fmt.Sprintf("examples: 1 to %d", labelEvalMaxExamples))
	}
	settings, err := s.core.LabelSettings(ctx)
	if err != nil {
		return 503, errorBody(503, "The project database is not available")
	}
	if settings.Backend == "" {
		return 409, errorBody(409, "choose a backend first (Settings, Model Labels), then evaluate it")
	}
	model, foundry := strings.CutPrefix(settings.Backend, "foundry:")
	if s.labels.backend == nil && (!foundry || runtime.GOOS != "windows") {
		return 409, errorBody(409, "label backend unavailable on this gateway")
	}
	inputs := make([]string, len(req.Examples))
	for i, e := range req.Examples {
		n := i + 1
		if strings.TrimSpace(e.Text) == "" {
			return 400, errorBody(400, fmt.Sprintf("example %d: text is empty", n))
		}
		inputs[i] = decide.CapInput(e.Text)
		req.Examples[i].Text = ""
		phase, activity := core.SafeWordLabel(e.Phase), core.SafeWordLabel(e.Activity)
		if (e.Phase != "" && !slices.Contains(core.PhaseWords, phase)) || (e.Activity != "" && !slices.Contains(settings.Activities, activity)) {
			return 400, errorBody(400, fmt.Sprintf("example %d: phase is one of %s, activity one of the configured list (%s)", n, strings.Join(core.PhaseWords, ", "), settings.ActivitiesText))
		}
		if phase == "" && activity == "" {
			return 400, errorBody(400, fmt.Sprintf("example %d: needs a phase or an activity", n))
		}
		req.Examples[i].Phase, req.Examples[i].Activity = phase, activity
	}
	if !s.labels.evalBusy.CompareAndSwap(false, true) {
		return 429, errorBody(429, "Another label evaluation is running on this gateway: wait for it to finish, then run this one again")
	}
	defer s.labels.evalBusy.Store(false)

	l := &s.labels
	budget := l.budget
	if budget == 0 {
		budget = labelBudget
	}
	runCtx, stop := context.WithTimeout(ctx, labelEvalBudget)
	defer stop()
	// Registered under the worker's lock: stop() (before the core closes) cancels and drains it; none starts once closed.
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return 503, errorBody(503, "The label subsystem is stopped (the gateway is shutting down)")
	}
	doneCh := make(chan struct{})
	l.evalCancel, l.evalDone = stop, doneCh
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.evalCancel, l.evalDone = nil, nil
		l.mu.Unlock()
		close(doneCh)
	}()
	start := time.Now()
	res := labelEvalResult{Backend: settings.Backend, Examples: len(req.Examples), Mistakes: map[string][]labelEvalConfusion{}, Wrong: []labelEvalWrong{}}
	var meter labelMeter
	confused := map[string]map[[2]string]int{"phase": {}, "activity": {}}
	for i, e := range req.Examples {
		done := false
		for attempt := 0; attempt < labelEvalAttempts && runCtx.Err() == nil && !done; attempt++ {
			decisionCtx, cancel := context.WithTimeout(runCtx, budget)
			var one labelMeter
			results, err := decideLabels(decisionCtx, s.meteredBackend(s.labelBackend(model), &one, cancel), settings.Activities, inputs[i], 0.67, e.Phase != "", e.Activity != "")
			ended := decisionCtx.Err() != nil // before cancel(): the decision ran out of its budget or the run ended
			cancel()
			meter.in, meter.out = meter.in+one.in, meter.out+one.out
			switch {
			case one.preempted:
				select { // the user's request has priority; the example waits a moment and is tried again
				case <-runCtx.Done():
				case <-time.After(200 * time.Millisecond):
				}
			case err != nil || ended:
				attempt = labelEvalAttempts // a failed example is not repeated: the run goes on
			default:
				done = true
				res.Run++
				for k, got := range results {
					kind, given, cells := "phase", e.Phase, &res.Phase
					if k == 1 {
						kind, given, cells = "activity", e.Activity, &res.Activity
					}
					if given == "" {
						continue
					}
					cells.Scored++
					choice := got.Choice
					if choice != "" {
						cells.Two.Decided++
						if labelScore(got) == 1000 {
							cells.One.Decided++
						}
						if choice == given {
							cells.Two.Correct++
							if labelScore(got) == 1000 {
								cells.One.Correct++
							}
						}
					}
					if choice != given {
						if choice != "" {
							confused[kind][[2]string{given, choice}]++
						}
						if len(res.Wrong) < labelEvalMaxWrong {
							res.Wrong = append(res.Wrong, labelEvalWrong{Kind: kind, Line: i + 1, Expected: given, Got: choice})
						}
					}
				}
			}
		}
	}
	res.NotRun = res.Examples - res.Run
	for kind, m := range confused {
		var list []labelEvalConfusion
		for pair, n := range m {
			list = append(list, labelEvalConfusion{Expected: pair[0], Got: pair[1], N: n})
		}
		sort.Slice(list, func(a, b int) bool {
			x, y := list[a], list[b]
			if x.N != y.N {
				return x.N > y.N
			}
			if x.Expected != y.Expected {
				return x.Expected < y.Expected
			}
			return x.Got < y.Got
		})
		res.Mistakes[kind] = append([]labelEvalConfusion{}, list[:min(len(list), labelEvalMaxConfused)]...)
	}
	// The worker's own accounting, once for the run. Nothing but numbers is handed to storage; its failure never fails the evaluation.
	l.mu.Lock()
	stopped := l.closed
	l.mu.Unlock()
	if !stopped { // stop() waits for this function: the store is still open here unless the subsystem was already stopped
		storeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.core.FinishLabelDecision(storeCtx, "ev_"+randomToken()[:16], "foundry", model, start, time.Now(), meter.in, meter.out, res.Run == 0, false)
		cancel()
	}
	if res.Run == 0 {
		if errors.Is(runCtx.Err(), context.Canceled) {
			return 499, errorBody(499, "The evaluation was cancelled")
		}
		return 502, errorBody(502, "The label backend gave no answer for any example: check that the model is available")
	}
	return 200, map[string]any{"eval": res}
}
