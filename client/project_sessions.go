package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"minigw/core"
	"net/url"
	"strconv"
	"strings"
)

func (e *projectEnv) projectSessions(args []string) error {
	fs := flag.NewFlagSet("project sessions", flag.ContinueOnError)
	days := fs.Int("days", 7, "days (1-400)")
	model := fs.String("model", "", "stored model ID")
	device := fs.String("device", "", "device ID")
	asJSON := fs.Bool("json", false, "JSON")
	phase := fs.String("phase", "", "only this phase")
	activity := fs.String("activity", "", "only this activity")
	task := fs.String("task", "", "only this task")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) > 1 {
		return errors.New("usage: minigw project sessions [<project>] [--days n] [--model m] [--device d] [--phase p] [--activity a] [--task t] [--json]")
	}
	var c connection
	var project string
	if len(pos) == 1 {
		c, _ = e.connect()
		project = pos[0]
	} else {
		var link projectLink
		c, link, err = e.linkedProject("")
		if err != nil {
			return err
		}
		project = strconv.FormatInt(link.Project, 10)
	}
	q := url.Values{"days": {strconv.Itoa(*days)}, "model": {*model}, "device": {*device}}
	for k, v := range map[string]string{"phase": *phase, "activity": *activity, "task": *task} {
		if v != "" {
			q.Set(k, v)
		}
	}
	page := core.SessionPage{Sessions: []core.SessionRow{}}
	seen := map[string]bool{}
	for {
		st, b, h := e.do(c, "GET", apiProjects+"/"+url.PathEscape(project)+"/sessions?"+q.Encode(), nil, nil)
		if st != 200 {
			return gatewayError(st, b, h)
		}
		var part core.SessionPage
		if err = json.Unmarshal(b, &part); err != nil {
			return err
		}
		page.Sessions = append(page.Sessions, part.Sessions...)
		if part.Next == "" {
			break
		}
		if seen[part.Next] {
			return errors.New("repeated session page")
		}
		seen[part.Next] = true
		q.Set("after", part.Next)
	}
	if *asJSON {
		b, _ := json.Marshal(page)
		e.say(string(b))
	} else {
		fmt.Fprint(e.out, sessionText(page))
	}
	return nil
}

func sessionText(page core.SessionPage) string {
	if len(page.Sessions) == 0 {
		return "no sessions\n"
	}
	metric := func(m core.SessionMetric) string {
		if m.Value == nil {
			return "unknown"
		}
		s := strconv.FormatInt(*m.Value, 10)
		if m.Unknown > 0 {
			s += "+?"
		}
		return s
	}
	cost := func(s *string) string {
		if s == nil {
			return "unknown"
		}
		return *s
	}
	var out strings.Builder
	for _, r := range page.Sessions {
		duration := "unknown"
		if r.DurationMs != nil {
			duration = strconv.FormatInt(*r.DurationMs, 10) + "ms"
		}
		end := "unknown"
		if r.End != nil {
			end = r.End.UTC().Format("2006-01-02T15:04:05Z")
		}
		totalCost := cost(r.Cost)
		if r.Cost != nil && r.UnknownCost > 0 {
			totalCost += "+?"
		}
		labels := ""
		for _, l := range []struct{ name, v, src string }{{"phase", r.Phase, ""}, {"activity", r.Activity, ""}, {"task", r.Task, r.TaskSource}} {
			if l.v != "" {
				labels += "  " + l.name + " " + clean(l.v)
				if l.src == "branch" {
					labels += " (branch)"
				}
			}
		}
		fmt.Fprintf(&out, "%s  %s  %s  %s  %s  %s  %d requests  %s in  %s out  %s cache-read  %s cache-write  USD %s (reported %s, estimated %s, %d unknown)  %s  end %s  wall %s%s\n", clean(r.ID), r.Start.UTC().Format("2006-01-02T15:04:05Z"), clean(r.UserName), clean(r.DeviceName), clean(strings.Join(r.Models, ",")), r.Outcome, r.Requests, metric(r.TokensIn), metric(r.TokensOut), metric(r.CacheRead), metric(r.CacheWrite), totalCost, cost(r.ReportedCost), cost(r.EstimatedCost), r.UnknownCost, strconv.FormatInt(r.ProjectID, 10), end, duration, labels)
	}
	return out.String()
}
