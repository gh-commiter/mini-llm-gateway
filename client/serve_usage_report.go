package main

// minigw gw usage: the provider/model report of GET /usage/events (totals per provider and model, per day, a total line),
// as text, CSV or the unchanged JSON. Mirrored by Sources/RouterCore/UsageReport.swift; the shared fixture
// client/testdata/usage-report.json is asserted by both (usage_report_test.go, UsageReportTests.swift).

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const usageReportHelp = `minigw gw usage [--days N] [--provider P] [--model M] [--json|--csv]   requests, tokens and cost per provider, model and day (UTC)
  minigw gw usage --legacy [--days N] [--device ID] [--folder DIR]   counters per folder, day (UTC) and agent (clear: sessions clear)`

type usageEventRow struct {
	Day, Provider, Model                                         string
	Events, TokensIn, TokensOut, FailedEvents, EventsWithoutCost int64
	ReportedCostUsd                                              *string
}

type usageEventGap struct {
	Count    int64
	From, To time.Time
}

// usageSum adds rows. Cost is the known (reported) part only; unknown counts the events that had no cost.
type usageSum struct {
	events, failed, in, out, unknown, nanos int64
	known                                   bool
}

func (s *usageSum) add(r usageEventRow) {
	s.events, s.failed, s.in, s.out, s.unknown = s.events+r.Events, s.failed+r.FailedEvents, s.in+r.TokensIn, s.out+r.TokensOut, s.unknown+r.EventsWithoutCost
	if r.ReportedCostUsd == nil {
		return
	}
	if n, ok := parseNanos(*r.ReportedCostUsd); ok {
		s.nanos, s.known = s.nanos+n, true
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
	if s.unknown > 0 {
		return fmt.Sprintf("%s (+%d unknown)", usd(s.nanos), s.unknown)
	}
	return usd(s.nanos)
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

func usageReportText(rows []usageEventRow, gaps []usageEventGap) string {
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
		case a.known != b.known:
			return a.known
		case a.nanos != b.nanos:
			return a.nanos > b.nanos
		case a.events != b.events:
			return a.events > b.events
		}
		return keys[i][0]+"\x00"+keys[i][1] < keys[j][0]+"\x00"+keys[j][1]
	})
	var mrows [][]string
	for _, k := range keys {
		s := groups[k]
		mrows = append(mrows, []string{oneLine(k[0]), oneLine(k[1]), strconv.FormatInt(s.events, 10), strconv.FormatInt(s.failed, 10), human(s.in), human(s.out), s.cost()})
	}
	names := make([]string, 0, len(days))
	for d := range days {
		names = append(names, d)
	}
	sort.Strings(names)
	var drows [][]string
	for _, d := range names {
		s := days[d]
		drows = append(drows, []string{oneLine(d), strconv.FormatInt(s.events, 10), human(s.in + s.out), s.cost()})
	}
	out := usageTable([]string{"provider", "model", "requests", "failed", "tokens in", "tokens out", "cost USD"}, mrows, []bool{false, false, true, true, true, true, false}) +
		"\n" + usageTable([]string{"day", "requests", "tokens in+out", "cost USD"}, drows, []bool{false, true, true, false}) +
		fmt.Sprintf("\ntotal: %d requests (%d failed), %s tokens in, %s tokens out, cost USD %s\n", total.events, total.failed, human(total.in), human(total.out), total.cost())
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
	out := "day,provider,model,requests,failed,tokens_in,tokens_out,reported_cost_usd,events_without_cost\n"
	for _, k := range keys {
		s := sums[k]
		cost := ""
		if s.known {
			cost = fmt.Sprintf("%d.%09d", s.nanos/1_000_000_000, s.nanos%1_000_000_000)
		}
		out += fmt.Sprintf("%s,%s,%s,%d,%d,%d,%d,%s,%d\n", csvCell(k[0], true), csvCell(k[1], true), csvCell(k[2], true), s.events, s.failed, s.in, s.out, cost, s.unknown)
	}
	return out
}
