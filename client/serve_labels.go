package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minigw/core"
	"minigw/decide"
)

const labelBudget = 20 * time.Second
const labelQueueBytes = 8 << 20

type labelJob struct {
	event      core.UsageEvent
	body       []byte
	input      string
	key        [32]byte
	generation uint64
	shadow     bool
}
type labelCacheEntry struct {
	results [2]decide.Result
	at      time.Time
}

type labelCache map[[32]byte]labelCacheEntry

type labelRate []time.Time

func (r *labelRate) take(now time.Time, limit int) bool {
	for len(*r) > 0 && now.Sub((*r)[0]) >= time.Hour {
		*r = (*r)[1:]
	}
	if len(*r) >= limit {
		return false
	}
	*r = append(*r, now)
	return true
}

func (c labelCache) expire(now time.Time) {
	for key, entry := range c {
		if now.Sub(entry.at) >= 6*time.Hour {
			delete(c, key)
		}
	}
}

func (c labelCache) put(key [32]byte, entry labelCacheEntry) {
	if _, exists := c[key]; !exists && len(c) >= 500 {
		var oldest [32]byte
		at := entry.at
		for k, v := range c {
			if !v.at.After(at) {
				oldest, at = k, v.at
			}
		}
		delete(c, oldest)
	}
	c[key] = entry
}

type labelState struct {
	mu             sync.Mutex
	settings       core.LabelSettings
	generation     uint64
	sampled        uint64
	queue          chan labelJob
	queueBytes     int
	cancel         context.CancelFunc
	decisionCancel context.CancelFunc
	done           chan struct{}
	closed         bool
	backend        decide.Backend // tests only; production calls Foundry directly
	budget         time.Duration  // tests may shorten the deadline
	skipped        atomic.Uint64
	failed         atomic.Uint64
}

// The tray can close and reopen the core without restarting the gateway.
func (l *labelState) open(settings core.LabelSettings) {
	l.mu.Lock()
	l.closed = false
	l.queue, l.done, l.cancel = nil, nil, nil
	l.queueBytes = 0
	l.mu.Unlock()
	l.configure(settings)
}

func (l *labelState) configure(s core.LabelSettings) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s.Activities = append([]string(nil), s.Activities...)
	s.LastError = ""
	a, _ := json.Marshal(l.settings)
	b, _ := json.Marshal(s)
	if string(a) != string(b) {
		if l.decisionCancel != nil {
			l.decisionCancel()
		}
		l.generation++
		l.settings = s
	}
}

func (l *labelState) stop() {
	l.mu.Lock()
	l.closed = true
	cancel, done := l.cancel, l.done
	if cancel != nil {
		cancel()
	}
	l.mu.Unlock()
	if done != nil {
		<-done
	}
}

// Admission only retains the existing body under a byte budget. Parsing, stripping and hashing run on the worker.
func (s *server) enqueueLabels(r *req) {
	e := r.event
	if e == nil || !r.evDispatched || r.URL.Path != "/v1/messages" || e.ParentID != "" || e.PhaseSource == core.LabelInternal || e.ActivitySource == core.LabelInternal || e.TaskSource == core.LabelInternal {
		return
	}
	l := &s.labels
	l.mu.Lock()
	if l.closed || !l.settings.Enabled || l.settings.Backend == "" || s.core == nil || (runtime.GOOS == "darwin" && l.backend == nil) {
		l.mu.Unlock()
		return
	}
	if l.queue != nil && len(l.queue) == cap(l.queue) {
		l.skipped.Add(1)
		l.mu.Unlock()
		return
	}
	if len(r.body) > labelQueueBytes-l.queueBytes {
		l.skipped.Add(1)
		l.mu.Unlock()
		return
	}
	generation := l.generation
	shadow := false
	if e.Phase != "" || e.Activity != "" {
		l.sampled++
		shadow = l.settings.Shadow && l.settings.ShadowRate > 0 && l.sampled%uint64(l.settings.ShadowRate) == 0
		if !shadow && e.Phase != "" && e.Activity != "" {
			l.mu.Unlock()
			return
		}
	}
	defer l.mu.Unlock()
	job := labelJob{event: *e, body: r.body, generation: generation, shadow: shadow}
	if l.queue == nil {
		l.queue = make(chan labelJob, 8)
		l.done = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		l.cancel = cancel
		go s.runLabels(ctx)
	}
	select {
	case l.queue <- job:
		l.queueBytes += len(job.body)
	default:
		l.skipped.Add(1)
	}
}

func labelInput(body []byte, device, task string) (string, [32]byte) {
	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &b) != nil {
		return "", [32]byte{}
	}
	for _, m := range b.Messages {
		if m.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
				return "", [32]byte{}
			}
			var parts []string
			for _, block := range blocks {
				if block.Type == "text" {
					parts = append(parts, block.Text)
				}
			}
			text = strings.Join(parts, "\n")
		}
		text = stripReminders(text)
		// Hash the whole first message; only the bounded prefix is retained in memory.
		key := sha256.Sum256([]byte(device + "\x00" + text + "\x00" + task))
		prefix := ""
		if task != "" {
			prefix = "task: " + core.SafeTaskLabel(task) + "\n"
		}
		return decide.CapInput(prefix + text), key
	}
	return "", [32]byte{}
}

var labelCriteria = map[string]string{
	"plan": "Plan work before execution.", "do": "Implement or execute the planned work.",
	"check": "Review or verify work and its results.", "act": "Correct the plan after a finding during execution or verification.",
	"feature": "Add new behavior.", "bugfix": "Fix incorrect behavior.", "security": "Protect access, secrets or trust boundaries.",
	"refactor": "Restructure code without changing behavior.", "docs": "Write or update documentation.",
	"test": "Create or run tests.", "release": "Prepare or publish a release.", "research": "Investigate a question.", "ops": "Operate or maintain infrastructure.",
}

func labelCandidates(words []string) []decide.Candidate {
	out := make([]decide.Candidate, 0, len(words))
	for _, id := range words {
		criteria := labelCriteria[id]
		if criteria == "" {
			criteria = id
		}
		out = append(out, decide.Candidate{ID: id, Criteria: criteria})
	}
	return out
}

func labelScore(r decide.Result) int { return int(math.Round(r.Scores[r.Choice] * 1000)) }

func (s *server) runLabels(ctx context.Context) {
	l := &s.labels
	defer close(l.done)
	defer func() {
		for {
			select {
			case job := <-l.queue:
				l.mu.Lock()
				l.queueBytes -= len(job.body)
				l.mu.Unlock()
			default:
				return
			}
		}
	}()
	cache := labelCache{}
	var cacheGeneration uint64
	var unavailableGeneration uint64
	var starts labelRate
	for {
		if ctx.Err() != nil {
			return
		}
		var job labelJob
		select {
		case <-ctx.Done():
			return
		case job = <-l.queue:
		}
		l.mu.Lock()
		l.queueBytes -= len(job.body)
		settings, generation := l.settings, l.generation
		l.mu.Unlock()
		if !settings.Enabled || job.generation != generation {
			l.skipped.Add(1)
			continue
		}
		if generation != cacheGeneration {
			clear(cache)
			cacheGeneration = generation
		}
		model, foundry := strings.CutPrefix(settings.Backend, "foundry:")
		if l.backend == nil && (!foundry || runtime.GOOS != "windows") {
			if unavailableGeneration != generation {
				_ = s.core.SetLabelError(ctx, "label backend unavailable on this gateway")
				unavailableGeneration = generation
			}
			l.skipped.Add(1)
			continue
		}
		job.input, job.key = labelInput(job.body, job.event.DeviceID, job.event.Task)
		job.body = nil
		if job.input == "" {
			l.skipped.Add(1)
			continue
		}
		now := time.Now()
		cache.expire(now)
		entry, hit := cache[job.key]
		var results [2]decide.Result
		if hit {
			results = entry.results
		} else {
			if !starts.take(now, settings.MaxPerHour) {
				l.skipped.Add(1)
				continue
			}
			budget := l.budget
			if budget == 0 {
				budget = labelBudget
			}
			decisionCtx, cancel := context.WithTimeout(ctx, budget)
			l.mu.Lock()
			if l.generation != generation {
				cancel()
			}
			l.decisionCancel = cancel
			l.mu.Unlock()
			backend := l.backend
			if backend == nil {
				backend = func(c context.Context, system, user string) (string, error) {
					return s.foundryComplete(c, model, system, user)
				}
			}
			var tokensIn, tokensOut int64
			counted := func(c context.Context, system, user string) (string, error) {
				tokensIn += int64((len(system) + len(user) + 3) / 4)
				reply, err := backend(c, system, user)
				tokensOut += int64((len(reply) + 3) / 4)
				return reply, err
			}
			threshold := 1.0
			if settings.Threshold == "0.67" {
				threshold = 0.67
			}
			var err error
			results[0], err = decide.Decide(decisionCtx, counted, decide.Request{Question: "Which phase describes this work?", Candidates: labelCandidates(core.PhaseWords), Input: job.input}, threshold)
			if err == nil {
				results[1], err = decide.Decide(decisionCtx, counted, decide.Request{Question: "Which activity describes this work?", Candidates: labelCandidates(settings.Activities), Input: job.input}, threshold)
			}
			if decisionCtx.Err() != nil {
				err = decide.ErrBackend
			}
			cancel()
			l.mu.Lock()
			l.decisionCancel = nil
			l.mu.Unlock()
			for i := range results {
				results[i].Model = model
			}
			// No backend error, reply or input is ever handed to storage or a logger.
			storeCtx, storeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			eventErr := s.core.FinishLabelDecision(storeCtx, "ev_"+randomToken()[:16], model, now, time.Now(), tokensIn, tokensOut, err != nil)
			storeCancel()
			if err != nil || eventErr != nil {
				l.mu.Lock()
				current := l.generation == generation && l.settings.Enabled
				l.mu.Unlock()
				if !current {
					l.skipped.Add(1)
					continue
				}
				l.failed.Add(1)
				if ctx.Err() == nil {
					_ = s.core.SetLabelError(ctx, "label decision failed")
				}
				continue
			}
			cache.put(job.key, labelCacheEntry{results: results, at: now})
		}
		if ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		current := l.generation == generation && l.settings.Enabled
		l.mu.Unlock()
		if !current {
			continue
		}
		// Begin/finish are asynchronous. Wait here, never on the request path.
		if !s.api.FlushRecorderContext(ctx) {
			return
		}
		if job.shadow {
			storageFailed := false
			for i, kind := range []string{"phase", "activity"} {
				given := job.event.Phase
				if i == 1 {
					given = job.event.Activity
				}
				if given == "" {
					continue
				}
				bucket := "unknown"
				switch labelScore(results[i]) {
				case 667:
					bucket = "667"
				case 1000:
					bucket = "1000"
				}
				if err := s.core.RecordAgreement(ctx, kind, bucket, results[i].Choice != "" && results[i].Choice == given); err != nil {
					storageFailed = true
					l.failed.Add(1)
					_ = s.core.SetLabelError(ctx, "label storage failed")
				}
			}
			if storageFailed {
				continue
			}
		} else {
			if _, err := s.core.SetModelLabels(ctx, job.event.ID, results[0].Choice, results[1].Choice, labelScore(results[0]), labelScore(results[1])); err != nil {
				l.failed.Add(1)
				_ = s.core.SetLabelError(ctx, "label storage failed")
				continue
			}
		}
		_ = s.core.SetLabelError(ctx, "")
	}
}
