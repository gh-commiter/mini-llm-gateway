package main

// minigw gw usage: the provider/model report of GET /usage/events (totals per provider and model, per day, a total line),
// as text, CSV or the unchanged JSON. Mirrored by Sources/RouterCore/UsageReport.swift; the shared fixture
// client/testdata/usage-report.json is asserted by both (usage_report_test.go, UsageReportTests.swift).

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const usageReportHelp = `minigw gw usage [--days N] [--provider P] [--model M] [--phase P] [--activity A] [--task T] [--label-source given|branch|model] [--sort failed] [--json|--csv]   requests, tokens, cost and failed agent time per provider, model and day (UTC)
  minigw gw usage --by phase|activity|task|project|model[,...] [--days N] [--phase P] [--activity A] [--task T] [--label-source given|branch|model] [--json]   the same numbers per phase, activity, task, project or model (one or two levels; at most 50 groups and an (other) row); unlabelled usage is its own row; --json gives these groups, not the raw rows
  minigw gw usage --legacy [--days N] [--device ID] [--folder DIR]   counters per folder, day (UTC) and agent (clear: sessions clear)`

type usageEventRow struct {
	Day, Provider, Model                                         string
	Project                                                      int64
	Phase, Activity, Task                                        string // only in a report asked with labels=1
	PhaseSource, ActivitySource, TaskSource                      string
	Events, TokensIn, TokensOut, FailedEvents, EventsWithoutCost int64
	CacheReadTokens, CacheWriteTokens                            int64
	EstimatedCostUsd                                             *string
	ReportedCostUsd                                              *string
	// Execution time (dispatch to finish) in ms of the events whose duration is known; failed = outcome error only.
	ErrorEvents, DurationMs, FailedDurationMs, UnknownDurationEvents int64
}

type usageEventGap struct {
	Count    int64
	From, To time.Time
}

// usageSum adds known reported and estimated amounts; unknown counts events without cost.
type usageSum struct {
	events, failed, in, out, unknown, nanos int64
	cachedIn, cachedOut                     int64
	errors, durationMs, failedMs, unknownMs int64
	known                                   bool
	estimated                               int64
	estimateKnown                           bool
	reported                                int64
	reportedKnown                           bool
}

func (s *usageSum) add(r usageEventRow) {
	s.events, s.failed, s.in, s.out, s.unknown = s.events+r.Events, s.failed+r.FailedEvents, s.in+r.TokensIn, s.out+r.TokensOut, s.unknown+r.EventsWithoutCost
	s.cachedIn, s.cachedOut = s.cachedIn+r.CacheReadTokens, s.cachedOut+r.CacheWriteTokens
	s.errors, s.durationMs, s.failedMs, s.unknownMs = s.errors+r.ErrorEvents, s.durationMs+r.DurationMs, s.failedMs+r.FailedDurationMs, s.unknownMs+r.UnknownDurationEvents
	if r.EstimatedCostUsd != nil {
		if n, ok := parseNanos(*r.EstimatedCostUsd); ok {
			s.estimated += n
			s.nanos += n
			s.estimateKnown = true
			s.known = true
		}
	}
	if r.ReportedCostUsd == nil {
		return
	}
	if n, ok := parseNanos(*r.ReportedCostUsd); ok {
		s.nanos, s.known = s.nanos+n, true
		s.reported += n
		s.reportedKnown = true
	} else {
		s.unknown += r.Events - r.EventsWithoutCost // malformed: not a known cost
	}
}

// parseNanos reads a decimal USD string ("0.006000000") as nanodollars; more than 9 decimals are cut off.
func parseNanos(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(whole) > 12 || strings.Trim(whole+frac, "0123456789") != "" {
		return 0, false
	}
	frac = (frac + "000000000")[:9]
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt(frac, 10, 64)
	return w*1_000_000_000 + f, true
}

// usd shows two decimals, four below one cent.
func usd(nanos int64) string {
	switch {
	case nanos == 0:
		return "0.00"
	case nanos < 100_000:
		return "<0.0001"
	case nanos < 10_000_000:
		return fmt.Sprintf("%d.%04d", (nanos+50_000)/1_000_000_000, (nanos+50_000)%1_000_000_000/100_000)
	}
	c := (nanos + 5_000_000) / 10_000_000
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

func (s usageSum) cost() string {
	if !s.known {
		return "-"
	}
	cost := usd(s.reported)
	if s.estimateKnown {
		cost = "~" + usd(s.estimated)
		if s.reportedKnown {
			cost = usd(s.reported) + " + " + cost
		}
	}
	if s.unknown > 0 {
		cost += fmt.Sprintf(" (+%d unknown)", s.unknown)
	}
	return cost
}

// minutes shows milliseconds as minutes with one decimal: 3480000 -> "58.0".
func minutes(ms int64) string {
	t := (ms + 3000) / 6000 // tenths of a minute, rounded
	return fmt.Sprintf("%d.%d", t/10, t%10)
}

// failedSummary: ", 7 errors, 58.0 failed agent minutes" for the total line; "" when no event failed with an error.
func (s usageSum) failedSummary() string {
	if s.errors == 0 {
		return ""
	}
	out := fmt.Sprintf(", %d errors", s.errors)
	if s.failedMs > 0 {
		out += ", " + minutes(s.failedMs) + " failed agent minutes"
	}
	if s.unknownMs > 0 {
		out += fmt.Sprintf(" (%d events of unknown duration)", s.unknownMs)
	}
	return out
}

// human: 914, 1.2K, 914K, 46.1M, 3B.
func human(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	for _, u := range []struct {
		div int64
		s   string
	}{{1_000, "K"}, {1_000_000, "M"}, {1_000_000_000, "B"}} {
		t := (n*10 + u.div/2) / u.div // tenths
		if t >= 10000 && u.s != "B" {
			continue
		}
		if t >= 1000 || t%10 == 0 {
			return fmt.Sprintf("%d%s", t/10, u.s)
		}
		return fmt.Sprintf("%d.%d%s", t/10, t%10, u.s)
	}
	return ""
}

// usageTable: columns two spaces apart; the first columns left, right[i] columns right-aligned; the last column is not padded.
func usageTable(header []string, rows [][]string, right []bool) string {
	all := append([][]string{header}, rows...)
	w := make([]int, len(header))
	for _, r := range all {
		for i, c := range r {
			w[i] = max(w[i], utf8.RuneCountInString(c))
		}
	}
	var b strings.Builder
	for _, r := range all {
		for i, c := range r {
			pad := strings.Repeat(" ", w[i]-utf8.RuneCountInString(c))
			switch {
			case i == len(r)-1:
				b.WriteString(c)
			case right[i]:
				b.WriteString(pad + c + "  ")
			default:
				b.WriteString(c + pad + "  ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func parseUsageReply(data []byte) (rows []usageEventRow, gaps []usageEventGap, err error) {
	var r struct {
		Days []usageEventRow
		Gaps []usageEventGap
	}
	err = json.Unmarshal(data, &r)
	return r.Days, r.Gaps, err
}

func usageReportText(rows []usageEventRow, gaps []usageEventGap, sortFailed bool) string {
	if len(rows) == 0 {
		return "no events\n"
	}
	var total usageSum
	groups, days := map[[2]string]*usageSum{}, map[string]*usageSum{}
	for _, r := range rows {
		k := [2]string{r.Provider, r.Model}
		if groups[k] == nil {
			groups[k] = &usageSum{}
		}
		if days[r.Day] == nil {
			days[r.Day] = &usageSum{}
		}
		groups[k].add(r)
		days[r.Day].add(r)
		total.add(r)
	}
	keys := make([][2]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := groups[keys[i]], groups[keys[j]]
		switch {
		case sortFailed && a.failedMs != b.failedMs:
			return a.failedMs > b.failedMs
		case a.known != b.known:
			return a.known
		case a.nanos != b.nanos:
			return a.nanos > b.nanos
		case a.events != b.events:
			return a.events > b.events
		}
		return keys[i][0]+"\x00"+keys[i][1] < keys[j][0]+"\x00"+keys[j][1]
	})
	withFailedTime := total.failedMs > 0 // the column appears when any model has failed agent minutes
	withCache := total.cachedIn > 0      // the column appears when any model read a prompt cache
	var mrows [][]string
	for _, k := range keys {
		s := groups[k]
		row := []string{oneLine(k[0]), oneLine(k[1]), strconv.FormatInt(s.events, 10), strconv.FormatInt(s.failed, 10)}
		if withFailedTime {
			ft := "-"
			if s.failedMs > 0 {
				ft = minutes(s.failedMs)
			}
			row = append(row, ft)
		}
		row = append(row, human(s.in))
		if withCache {
			row = append(row, human(s.cachedIn))
		}
		mrows = append(mrows, append(row, human(s.out), s.cost()))
	}
	names := make([]string, 0, len(days))
	for d := range days {
		names = append(names, d)
	}
	sort.Strings(names)
	var drows [][]string
	for _, d := range names {
		s := days[d]
		row := []string{oneLine(d), strconv.FormatInt(s.events, 10), human(s.in + s.out)}
		if withCache {
			row = append(row, human(s.cachedIn))
		}
		drows = append(drows, append(row, s.cost()))
	}
	head, right := []string{"provider", "model", "requests", "failed"}, []bool{false, false, true, true}
	if withFailedTime {
		head, right = append(head, "failed min"), append(right, true)
	}
	head, right = append(head, "tokens in"), append(right, true)
	if withCache {
		head, right = append(head, "cached in"), append(right, true)
	}
	head, right = append(head, "tokens out", "cost USD"), append(right, true, false)
	dhead, dright := []string{"day", "requests", "tokens in+out"}, []bool{false, true, true}
	if withCache {
		dhead, dright = append(dhead, "cached in"), append(dright, true)
	}
	dhead, dright = append(dhead, "cost USD"), append(dright, false)
	totalIn := human(total.in) + " tokens in, "
	if withCache {
		totalIn += human(total.cachedIn) + " cached in, "
	}
	out := usageTable(head, mrows, right) +
		"\n" + usageTable(dhead, drows, dright) +
		fmt.Sprintf("\ntotal: %d requests (%d failed), %s%s tokens out, cost USD %s%s\n", total.events, total.failed, totalIn, human(total.out), total.cost(), total.failedSummary())
	for _, g := range gaps {
		out += fmt.Sprintf("%d events lost between %s and %s (UTC)\n", g.Count, g.From.UTC().Format("2006-01-02 15:04"), g.To.UTC().Format("2006-01-02 15:04"))
	}
	return out
}

// csvCell quotes commas, quotes and newlines; a text cell that starts like a formula gets a leading apostrophe.
func csvCell(s string, text bool) string {
	if text && s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		s = "'" + s
	}
	if strings.ContainsAny(s, ",\"\n\r") {
		s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// usageReportCSV: one row per day, provider and model (the users and projects of the API rows are added up).
func usageReportCSV(rows []usageEventRow) string {
	sums := map[[3]string]*usageSum{}
	var keys [][3]string
	for _, r := range rows {
		k := [3]string{r.Day, r.Provider, r.Model}
		if sums[k] == nil {
			sums[k] = &usageSum{}
			keys = append(keys, k)
		}
		sums[k].add(r)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.Join(keys[i][:], "\x00") < strings.Join(keys[j][:], "\x00") })
	out := "day,provider,model,requests,failed,tokens_in,tokens_out,reported_cost_usd,estimated_cost_usd,events_without_cost,errors,duration_ms,failed_duration_ms,unknown_duration_events,cache_read_tokens,cache_write_tokens\n"
	for _, k := range keys {
		s := sums[k]
		cost := ""
		if s.reportedKnown {
			cost = fmt.Sprintf("%d.%09d", s.reported/1_000_000_000, s.reported%1_000_000_000)
		}
		estimated := ""
		if s.estimateKnown {
			estimated = fmt.Sprintf("%d.%09d", s.estimated/1_000_000_000, s.estimated%1_000_000_000)
		}
		out += fmt.Sprintf("%s,%s,%s,%d,%d,%d,%d,%s,%s,%d,%d,%d,%d,%d,%d,%d\n", csvCell(k[0], true), csvCell(k[1], true), csvCell(k[2], true), s.events, s.failed, s.in, s.out, cost, estimated, s.unknown, s.errors, s.durationMs, s.failedMs, s.unknownMs, s.cachedIn, s.cachedOut)
	}
	return out
}

// usageByDims are the dimensions of minigw gw usage --by.
var usageByDims = []string{"phase", "activity", "task", "project", "model"}

// parseUsageBy checks --by: one or two different dimensions, comma separated.
func parseUsageBy(v string) ([]string, bool) {
	parts := strings.Split(v, ",")
	if len(parts) < 1 || len(parts) > 2 {
		return nil, false
	}
	for i, d := range parts {
		if !slices.Contains(usageByDims, d) || i == 1 && d == parts[0] {
			return nil, false
		}
	}
	return parts, true
}

// byLabels: does --by need the rows split by label (labels=1)?
func byLabels(by []string) bool {
	return slices.Contains(by, "phase") || slices.Contains(by, "activity") || slices.Contains(by, "task")
}

const unlabelled = "(unlabelled)"

// maxByGroups: the table and the grouped JSON show this many groups; the rest is one "(other)" row, so shares add up.
const maxByGroups = 50

// usageByValue is the value of one dimension of a row, with the source of a label: the display text adds " (branch)" to a
// task taken from the branch and " (model)" or " (internal)" to a phase or activity chosen by a model or the gateway; raw is the value alone ("(unlabelled)" for no label).
func usageByValue(r usageEventRow, dim string, projects map[int64]string) (display, raw, source string) {
	v := ""
	switch dim {
	case "phase":
		v, source = r.Phase, modelSource(r.PhaseSource)
	case "activity":
		v, source = r.Activity, modelSource(r.ActivitySource)
	case "task":
		v, source = r.Task, r.TaskSource
	case "model":
		return r.Model, r.Model, ""
	case "project":
		switch {
		case r.Project == 0:
			v = "(no project)"
		case projects[r.Project] != "":
			v = projects[r.Project]
		default:
			v = strconv.FormatInt(r.Project, 10)
		}
		return v, v, ""
	}
	if v == "" {
		return unlabelled, unlabelled, ""
	}
	if source == "branch" || source == "model" || source == "internal" {
		return v + " (" + source + ")", v, source
	}
	return v, v, source
}

// modelSource: of a phase or activity only a label a model chose, or the gateway's own event, shows its source (a given one
// is the plain value, as before labels could be filled by a model).
func modelSource(src string) string {
	if src == "model" || src == "internal" {
		return src
	}
	return ""
}

type byGroup struct {
	vals, raw, src [2]string
	sum            usageSum
}

// usageByGroups: the rows summed per one or two dimensions, ordered by the first dimension's cost (the unlabelled one
// last), at most maxByGroups groups and one "(other)" group for the rest; with the total of all rows.
func usageByGroups(rows []usageEventRow, by []string, projects map[int64]string) (groups []byGroup, total usageSum) {
	byKey := map[[2]string]*byGroup{}
	for _, r := range rows {
		var g byGroup
		for i, d := range by {
			g.vals[i], g.raw[i], g.src[i] = usageByValue(r, d, projects)
		}
		k := g.vals
		if byKey[k] == nil {
			byKey[k] = &g
		}
		byKey[k].sum.add(r)
		total.add(r)
	}
	keys := make([][2]string, 0, len(byKey))
	first := map[string]*usageSum{} // the totals of the first dimension: they order the groups
	for k, g := range byKey {
		keys = append(keys, k)
		if first[k[0]] == nil {
			first[k[0]] = &usageSum{}
		}
		first[k[0]].events += g.sum.events
		first[k[0]].nanos += g.sum.nanos
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a[0] != b[0] {
			x, y := first[a[0]], first[b[0]]
			switch {
			case (a[0] == unlabelled) != (b[0] == unlabelled): // the unlabelled row is last, never hidden
				return b[0] == unlabelled
			case x.nanos != y.nanos:
				return x.nanos > y.nanos
			case x.events != y.events:
				return x.events > y.events
			}
			return a[0] < b[0]
		}
		x, y := byKey[a].sum, byKey[b].sum
		if x.nanos != y.nanos {
			return x.nanos > y.nanos
		}
		if x.events != y.events {
			return x.events > y.events
		}
		return a[1] < b[1]
	})
	var other *byGroup
	for i, k := range keys {
		if i < maxByGroups {
			groups = append(groups, *byKey[k])
			continue
		}
		if other == nil {
			other = &byGroup{vals: [2]string{"(other)", ""}, raw: [2]string{"(other)", ""}}
			if len(by) > 1 {
				other.vals[1], other.raw[1] = "", ""
			}
		}
		other.sum.merge(byKey[k].sum)
	}
	if other != nil {
		groups = append(groups, *other)
	}
	return groups, total
}

// merge adds another sum (the groups folded into "(other)").
func (s *usageSum) merge(o usageSum) {
	s.events, s.failed, s.in, s.out, s.unknown, s.nanos = s.events+o.events, s.failed+o.failed, s.in+o.in, s.out+o.out, s.unknown+o.unknown, s.nanos+o.nanos
	s.cachedIn, s.cachedOut = s.cachedIn+o.cachedIn, s.cachedOut+o.cachedOut
	s.errors, s.durationMs, s.failedMs, s.unknownMs = s.errors+o.errors, s.durationMs+o.durationMs, s.failedMs+o.failedMs, s.unknownMs+o.unknownMs
	s.known = s.known || o.known
	s.estimated, s.reported = s.estimated+o.estimated, s.reported+o.reported
	s.estimateKnown, s.reportedKnown = s.estimateKnown || o.estimateKnown, s.reportedKnown || o.reportedKnown
}

func usagePercent(events, all int64) int64 {
	if all == 0 {
		return 0
	}
	return (events*100 + all/2) / all
}

// usageByText: the usage numbers per one or two dimensions, a share column (the row's requests of all requests) and a
// total line; truncated says the API cut the report.
func usageByText(rows []usageEventRow, gaps []usageEventGap, by []string, projects map[int64]string, truncated bool) string {
	if len(rows) == 0 {
		return "no events\n"
	}
	groups, total := usageByGroups(rows, by, projects)
	withFailedTime, withCache := total.failedMs > 0, total.cachedIn > 0
	head, right := append([]string{}, by...), make([]bool, len(by))
	head, right = append(head, "requests", "share", "failed"), append(right, true, true, true)
	if withFailedTime {
		head, right = append(head, "failed min"), append(right, true)
	}
	head, right = append(head, "tokens in"), append(right, true)
	if withCache {
		head, right = append(head, "cached in"), append(right, true)
	}
	head, right = append(head, "tokens out", "cost USD"), append(right, true, false)
	var table [][]string
	for _, g := range groups {
		s := g.sum
		row := []string{oneLine(g.vals[0])}
		if len(by) > 1 {
			row = append(row, oneLine(g.vals[1]))
		}
		row = append(row, strconv.FormatInt(s.events, 10), fmt.Sprintf("%d%%", usagePercent(s.events, total.events)), strconv.FormatInt(s.failed, 10))
		if withFailedTime {
			ft := "-"
			if s.failedMs > 0 {
				ft = minutes(s.failedMs)
			}
			row = append(row, ft)
		}
		row = append(row, human(s.in))
		if withCache {
			row = append(row, human(s.cachedIn))
		}
		table = append(table, append(row, human(s.out), s.cost()))
	}
	totalIn := human(total.in) + " tokens in, "
	if withCache {
		totalIn += human(total.cachedIn) + " cached in, "
	}
	out := usageTable(head, table, right) + fmt.Sprintf("\ntotal: %d requests (%d failed), %s%s tokens out, cost USD %s%s\n", total.events, total.failed, totalIn, human(total.out), total.cost(), total.failedSummary())
	if truncated {
		out += "the report was cut: only the newest 5000 rows were returned; narrow it with --days or a filter\n"
	}
	for _, g := range gaps {
		out += fmt.Sprintf("%d events lost between %s and %s (UTC)\n", g.Count, g.From.UTC().Format("2006-01-02 15:04"), g.To.UTC().Format("2006-01-02 15:04"))
	}
	return out
}

// usageByJSON: the same groups and numbers as the table, as JSON (keys sorted); costs are decimal USD strings or null.
func usageByJSON(rows []usageEventRow, gaps []usageEventGap, by []string, projects map[int64]string, truncated bool) string {
	groups, total := usageByGroups(rows, by, projects)
	nanos := func(known bool, n int64) any {
		if !known {
			return nil
		}
		return fmt.Sprintf("%d.%09d", n/1_000_000_000, n%1_000_000_000)
	}
	numbers := func(s usageSum) map[string]any {
		return map[string]any{"requests": s.events, "share": usagePercent(s.events, total.events), "failed": s.failed, "errors": s.errors, "tokensIn": s.in, "tokensOut": s.out,
			"cacheReadTokens": s.cachedIn, "failedDurationMs": s.failedMs, "eventsWithoutCost": s.unknown,
			"reportedCostUsd": nanos(s.reportedKnown, s.reported), "estimatedCostUsd": nanos(s.estimateKnown, s.estimated)}
	}
	var gs []map[string]any
	for _, g := range groups {
		m := numbers(g.sum)
		for i, d := range by {
			m[d] = g.raw[i]
			if g.src[i] != "" {
				m[d+"Source"] = g.src[i]
			}
		}
		gs = append(gs, m)
	}
	gl := []map[string]any{}
	for _, g := range gaps {
		gl = append(gl, map[string]any{"count": g.Count, "from": g.From.UTC().Format(time.RFC3339), "to": g.To.UTC().Format(time.RFC3339)})
	}
	if gs == nil {
		gs = []map[string]any{}
	}
	out := map[string]any{"by": by, "groups": gs, "total": numbers(total), "gaps": gl, "timezone": "UTC"}
	if truncated {
		out["truncated"] = true
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// usageTruncated: did the API cut a report split by label?
func usageTruncated(data []byte) bool {
	var r struct{ Truncated bool }
	return json.Unmarshal(data, &r) == nil && r.Truncated
}
