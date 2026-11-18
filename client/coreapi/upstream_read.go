package coreapi

import (
	"context"
	"strings"
	"time"

	"minigw/core"
)

// adminUpstreamPull serves POST /upstream-pull {project or team, upstream, dryRun}: the owner's explicit pull. dryRun only asks the upstream
// for a manifest and says what it would let this gateway read (nothing is written); a real pull fills the mirror of the links with reading
// on (at most a bounded number of notes per call: it answers more:true and the caller asks again).
func (a *API) adminUpstreamPull(ctx context.Context, st *core.Store, method string, body []byte, failErr func(error) (int, any, bool)) (int, any, bool) {
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	h := a.upstreamHost(ctx)
	if h == nil || !h.Available() {
		return fail(501, "this gateway cannot read from an upstream here")
	}
	if method != "POST" {
		return fail(404, "not found")
	}
	var b struct {
		Project  int64  `json:"project"`
		Team     int64  `json:"team"`
		Upstream string `json:"upstream"`
		DryRun   bool   `json:"dryRun"`
	}
	if decodeStrict(body, &b) != nil || b.Project < 0 || b.Team < 0 || b.Project > 0 && b.Team > 0 || b.Upstream != "" && !core.ValidUpstreamName(b.Upstream) {
		return fail(400, "bad JSON body (project or team, upstream, dryRun)")
	}
	sleep := a.o.UpstreamSleep
	if sleep == nil {
		sleep = ctxSleep(ctx)
	}
	if a.o.UpstreamBudget > 0 { // one call of a helper has a time limit
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.o.UpstreamBudget)
		defer cancel()
		inner := sleep
		sleep = func(d time.Duration) bool {
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d+time.Second {
				return false
			}
			return inner(d)
		}
	}
	res, err := st.PullUpstream(ctx, h, b.Project, b.Team, b.Upstream, b.DryRun, sleep)
	if err != nil {
		return failErr(err)
	}
	rows := []adminPullJSON{}
	for _, r := range res {
		rows = append(rows, adminPullJSON{r, strings.Join(r.Lines, "\n")})
	}
	return 200, map[string]any{"pulls": rows}, true
}

// adminPullJSON is a pull report with its lines as one text (as adminPushJSON).
type adminPullJSON struct {
	core.UpstreamPull
	LinesText string `json:"linesText"`
}
