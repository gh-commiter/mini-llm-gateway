package core

import (
	"database/sql"
	"regexp"
	"strings"
)

// Labels of a run for usage reports: the phase (plan, do, check, act), the activity (feature, bugfix, ...) and a task
// reference. They are data, never permissions: a missing label is empty, an unknown value is kept as given when it is
// a safe token, and a value that is not one is dropped (the event is never refused for a label). Each label has a
// source: "given" (the caller said it), "branch" (the client read the git branch; task only), "" (none); "classified"
// is reserved for a later step.
var (
	labelWordRe = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)
	labelTaskRe = regexp.MustCompile(`^[A-Za-z0-9._#/-]{1,64}$`)
)

// SafeWordLabel lowercases a phase or activity and returns it when it is a safe token, else "".
func SafeWordLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if labelWordRe.MatchString(s) {
		return s
	}
	return ""
}

// SafeTaskLabel returns the task when it is a safe token, else "".
func SafeTaskLabel(s string) string {
	s = strings.TrimSpace(s)
	if labelTaskRe.MatchString(s) {
		return s
	}
	return ""
}

func labelSource(label, src string) string {
	switch {
	case label == "":
		return ""
	case src == "branch":
		return "branch"
	}
	return "given"
}

// normalizeLabels applies the gateway's own limits to the labels of an event (a modified client cannot store more).
func normalizeLabels(e *UsageEvent) {
	e.Phase, e.Activity, e.Task = SafeWordLabel(e.Phase), SafeWordLabel(e.Activity), SafeTaskLabel(e.Task)
	e.PhaseSource, e.ActivitySource, e.TaskSource = labelSource(e.Phase, "given"), labelSource(e.Activity, "given"), labelSource(e.Task, e.TaskSource)
}

// migrate17: labels of an event. Old rows have none (empty). Folded daily totals carry no labels.
func migrate17(tx *sql.Tx) error {
	for _, c := range []string{"phase", "activity", "task", "phase_source", "activity_source", "task_source"} {
		if _, err := tx.Exec(`ALTER TABLE usage_events ADD COLUMN ` + c + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// normalizeLabelFilters applies to filter values the normalization stored values got (phase and activity lower case): an
// empty value is no filter; a value that is not a safe token can match nothing (impossible), it is not an error.
func normalizeLabelFilters(phase, activity, task string) (p, a, t string, impossible bool) {
	norm := func(v string, safe func(string) string) string {
		if v == "" {
			return ""
		}
		if s := safe(v); s != "" {
			return s
		}
		impossible = true
		return ""
	}
	return norm(phase, SafeWordLabel), norm(activity, SafeWordLabel), norm(task, SafeTaskLabel), impossible
}
