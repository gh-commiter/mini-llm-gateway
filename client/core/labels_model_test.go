package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (f *fx) labelsOf(id string) (a [6]string, s [2]int) {
	f.t.Helper()
	var ps, as *int
	f.ok(f.DB().QueryRow(`SELECT phase,activity,task,phase_source,activity_source,task_source,phase_score,activity_score FROM usage_events WHERE id=?`, id).Scan(&a[0], &a[1], &a[2], &a[3], &a[4], &a[5], &ps, &as))
	if ps != nil {
		s[0] = *ps
	}
	if as != nil {
		s[1] = *as
	}
	return
}

func TestMigration20To23AddsLabelsImportObjectsAndTraversal(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	for _, m := range migrations[:20] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	setMeta(tx, "schema", "20")
	if _, err = tx.Exec(`INSERT INTO usage_events(id,start,end,device_id,provider,model,outcome,token_source,phase,phase_source) VALUES('old',?,?,'d','p','m','ok','unknown','do','given')`, ms(time.Now()), ms(time.Now())); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	db.Close()
	s := openT(t, dir)
	defer s.Close()
	var phase, src string
	var score *int
	if err = s.DB().QueryRow(`SELECT phase,phase_source,phase_score FROM usage_events WHERE id='old'`).Scan(&phase, &src, &score); err != nil || phase != "do" || src != "given" || score != nil {
		t.Fatal(phase, src, score, err) // an old row is unchanged, with no score
	}
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-20-*.db")); len(snaps) != 1 {
		t.Fatal("no snapshot of schema 20", snaps)
	}
	if v, _ := schemaVersion(ctx, s.DB()); v != 24 || len(migrations) != 24 {
		t.Fatal("schema after 20 -> 24:", v, len(migrations)) // 21 labels, 22 import objects, 23 push traversal, 24 upstream mirror
	}
	if _, err = s.DB().Exec(`SELECT source,kind,id,name,adopted FROM import_objects`); err != nil {
		t.Fatal(err)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.LabelSettings(ctx); l.Enabled || l.Backend != "" || !l.Shadow || l.Threshold != "1.0" {
		t.Fatalf("the settings are not off by default: %+v", l)
	}
}

func TestModelLabelsNeverOverwriteAndStayInTheList(t *testing.T) {
	f := newFx(t)
	now := f.now()
	f.labelled("root", f.uid["ana"], f.pid["fakture"], "m", "plan", "", "", "", now) // phase given, activity empty
	kid := func(id, phase string) {
		f.ok(f.BeginEvent(ctx, UsageEvent{ID: id, ParentID: "root", Start: now, UserID: f.uid["ana"], ProjectID: f.pid["fakture"], DeviceID: "dev", Provider: "codex", Model: "m", Phase: phase}))
	}
	kid("kid1", "")
	kid("kid2", "check")
	// the model says do/bugfix: the given phase of the root stays, the empty activity is filled
	n, err := f.SetModelLabels(ctx, "root", "do", "bugfix", 1000, 667)
	f.ok(err)
	if a, s := f.labelsOf("root"); a != [6]string{"plan", "bugfix", "", "given", "model", ""} || s != [2]int{0, 667} || n < 2 {
		t.Fatal(a, s, n)
	}
	// children get the labels their own source leaves empty; an explicit one stays
	if a, s := f.labelsOf("kid1"); a != [6]string{"do", "bugfix", "", "model", "model", ""} || s != [2]int{1000, 667} {
		t.Fatal(a, s)
	}
	if a, _ := f.labelsOf("kid2"); a[0] != "check" || a[3] != "given" || a[1] != "bugfix" {
		t.Fatal(a)
	}
	// a second decision never replaces a model label either
	_, err = f.SetModelLabels(ctx, "root", "act", "docs", 667, 667)
	f.ok(err)
	if a, _ := f.labelsOf("kid1"); a[0] != "do" || a[1] != "bugfix" {
		t.Fatal(a)
	}
	// the gateway's own event (source internal) is never touched
	f.labelled("int", f.uid["ana"], f.pid["fakture"], "m", "", "", "", "", now)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE usage_events SET activity='ops', activity_source='internal', task='usage-labels', task_source='internal' WHERE id='int'`)
		return err
	}))
	f.ok2(f.SetModelLabels(ctx, "int", "plan", "docs", 1000, 1000))
	if a, _ := f.labelsOf("int"); a[0] != "" || a[1] != "ops" || a[4] != "internal" {
		t.Fatal(a)
	}
	// a value outside the lists is dropped, a score outside 1..1000 too
	f.labelled("r2", f.uid["ana"], f.pid["fakture"], "m", "", "", "", "", now)
	f.ok2(f.SetModelLabels(ctx, "r2", "remediate", "docs", 1000, 0))
	if a, _ := f.labelsOf("r2"); a != [6]string{} {
		t.Fatal(a)
	}
	f.ok2(f.SetModelLabels(ctx, "r2", "do", "docs", 1000, 1000))
	if a, _ := f.labelsOf("r2"); a[0] != "do" || a[1] != "docs" {
		t.Fatal(a)
	}
	// the activity list is the owner's
	f.labelled("r3", f.uid["ana"], f.pid["fakture"], "m", "", "", "", "", now)
	_, err = f.SetLabelSettings(ctx, LabelSettingsPatch{Activities: json.RawMessage(`"audit, Docs"`)})
	f.ok(err)
	f.ok2(f.SetModelLabels(ctx, "r3", "do", "feature", 1000, 1000))
	if a, _ := f.labelsOf("r3"); a[1] != "" {
		t.Fatal("an activity that left the list was stored:", a)
	}
	f.ok2(f.SetModelLabels(ctx, "r3", "do", "audit", 1000, 1000))
	if a, _ := f.labelsOf("r3"); a[1] != "audit" {
		t.Fatal(a)
	}
	// only a root event; an unknown id is not found
	if _, err = f.SetModelLabels(ctx, "kid1", "do", "docs", 1000, 1000); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = f.SetModelLabels(ctx, "nope", "do", "docs", 1000, 1000); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// a finish that arrives later (replay) keeps the model labels
	e := UsageEvent{ID: "r2", Start: now, DeviceID: "dev", Provider: "codex", Model: "m", End: now.Add(time.Second), Outcome: OutcomeOK, TokensIn: 1, TokensOut: 1, TokenSource: "reported"}
	f.ok(f.FinishEvent(ctx, e))
	if a, _ := f.labelsOf("r2"); a[3] != "model" {
		t.Fatal(a)
	}
}

func (f *fx) ok2(_ int64, err error) { f.t.Helper(); f.ok(err) }

// A client can claim given and branch, never model or internal.
func TestClientCannotClaimModelSource(t *testing.T) {
	f := newFx(t)
	e := UsageEvent{ID: "c1", Start: f.now(), DeviceID: "dev", Provider: "codex", Model: "m", Phase: "do", PhaseSource: "model", Activity: "docs", ActivitySource: "internal", Task: "x", TaskSource: "model"}
	f.ok(f.BeginEvent(ctx, e))
	if a, s := f.labelsOf("c1"); a != [6]string{"do", "docs", "x", "given", "given", "given"} || s != [2]int{} {
		t.Fatal(a, s)
	}
}

func TestLabelSettings(t *testing.T) {
	f := newFx(t)
	str := func(v string) *string { return &v }
	yes := true
	// turning on needs a backend
	if _, err := f.SetLabelSettings(ctx, LabelSettingsPatch{Enabled: &yes}); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatal(err)
	}
	for _, bad := range []string{"grok", "foundry:", "foundry:a b", "apple-fm ", "http://x"} {
		if _, err := f.SetLabelSettings(ctx, LabelSettingsPatch{Backend: str(bad)}); err == nil {
			t.Fatal("accepted backend", bad)
		}
	}
	l, err := f.SetLabelSettings(ctx, LabelSettingsPatch{Enabled: &yes, Backend: str("foundry:phi-4-mini"), Threshold: json.RawMessage(`0.67`), MaxPerHour: json.RawMessage(`"60"`), ShadowRate: json.RawMessage(`2`)})
	f.ok(err)
	if !l.Enabled || l.Backend != "foundry:phi-4-mini" || l.Threshold != "0.67" || l.MaxPerHour != 60 || l.ShadowRate != 2 || !l.Shadow {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []LabelSettingsPatch{{Threshold: json.RawMessage(`0.5`)}, {Threshold: json.RawMessage(`"x"`)}, {MaxPerHour: json.RawMessage(`0`)}, {MaxPerHour: json.RawMessage(`1001`)}, {ShadowRate: json.RawMessage(`"abc"`)}, {Activities: json.RawMessage(`"a b"`)},
		{Activities: json.RawMessage(`["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]`)}} {
		if _, err := f.SetLabelSettings(ctx, bad); err == nil {
			t.Fatalf("accepted %s", mustJSON(bad))
		}
	}
	// a partial change keeps the rest; the activity list: words, no repeats, "" = the default list
	no := false
	l, err = f.SetLabelSettings(ctx, LabelSettingsPatch{Shadow: &no, Activities: json.RawMessage(`"Audit, audit,docs"`)})
	f.ok(err)
	if l.Backend != "foundry:phi-4-mini" || l.Shadow || strings.Join(l.Activities, ",") != "audit,docs" || l.ActivitiesText != "audit, docs" {
		t.Fatalf("%+v", l)
	}
	l, err = f.SetLabelSettings(ctx, LabelSettingsPatch{Activities: json.RawMessage(`""`)})
	f.ok(err)
	if len(l.Activities) != len(DefaultActivities) {
		t.Fatalf("%+v", l)
	}
	// lastError is the worker's; a new backend clears it
	f.ok(f.SetLabelError(ctx, strings.Repeat("x", 500)))
	if l, _ = f.LabelSettings(ctx); len(l.LastError) != 200 {
		t.Fatal(len(l.LastError))
	}
	l, err = f.SetLabelSettings(ctx, LabelSettingsPatch{Backend: str("apple-fm")})
	f.ok(err)
	if l.LastError != "" {
		t.Fatal(l.LastError)
	}
	// the change is in the audit trail, with no value of any prompt
	var n int
	f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='labels.settings'`).Scan(&n)
	if n < 3 {
		t.Fatal("no audit rows", n)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestLabelAgreement(t *testing.T) {
	f := newFx(t)
	day1 := f.now().UTC().Format("2006-01-02")
	f.ok(f.RecordAgreement(ctx, "phase", "1000", true))
	f.ok(f.RecordAgreement(ctx, "phase", "1000", true))
	f.ok(f.RecordAgreement(ctx, "phase", "1000", false))
	f.ok(f.RecordAgreement(ctx, "activity", "unknown", false))
	f.clock = f.clock.AddDate(0, 0, 40)
	day2 := f.now().UTC().Format("2006-01-02")
	f.ok(f.RecordAgreement(ctx, "phase", "667", true))
	rows, err := f.Agreement(ctx, 30)
	f.ok(err)
	if len(rows) != 1 || rows[0] != (AgreementRow{"phase", "667", 1, 1, "phase/667", "100 %", "1", day2}) { // 40 days ago is outside the 30 days
		t.Fatalf("%+v", rows)
	}
	rows, _ = f.Agreement(ctx, 60)
	if len(rows) != 3 || rows[0] != (AgreementRow{"activity", "unknown", 0, 1, "activity/unknown", "0 %", "1", day1}) || rows[1] != (AgreementRow{"phase", "1000", 2, 3, "phase/1000", "67 %", "3", day1}) {
		t.Fatalf("%+v", rows)
	}
	for _, c := range [][3]string{{"task", "1000", ""}, {"phase", "500", ""}, {"", "", ""}} {
		if err := f.RecordAgreement(ctx, c[0], c[1], true); err == nil {
			t.Fatal("accepted", c)
		}
	}
	if _, err := f.Agreement(ctx, 401); err == nil {
		t.Fatal("days 401")
	}
}

func TestLabelSourceFilterOnlyNarrows(t *testing.T) {
	f := newFx(t)
	now := f.now()
	f.labelled("g", f.uid["ana"], f.pid["fakture"], "m", "do", "", "", "", now)
	f.labelled("m1", f.uid["ana"], f.pid["fakture"], "m", "", "", "", "", now)
	f.labelled("o", f.uid["bor"], f.pid["fakture"], "m", "", "", "", "", now)
	f.ok2(f.SetModelLabels(ctx, "m1", "plan", "", 1000, 0))
	f.ok2(f.SetModelLabels(ctx, "o", "plan", "", 1000, 0))
	p := f.p("ana")
	rep, err := f.Usage(ctx, p, 0, 7, LabelQuery{LabelSource: "model", Labels: true})
	f.ok(err)
	if len(rep.Rows) != 1 || rep.Rows[0].Events != 1 || rep.Rows[0].PhaseSource != "model" { // ana's own model event only, not bor's
		t.Fatalf("%+v", rep.Rows)
	}
	all, err := f.AdminUsage(ctx, 7, UsageFilter{LabelSource: "given"})
	f.ok(err)
	if len(all.Rows) != 1 || all.Rows[0].Events != 1 {
		t.Fatalf("%+v", all.Rows)
	}
	if _, err = f.AdminUsage(ctx, 7, UsageFilter{LabelSource: "classified"}); err == nil {
		t.Fatal("a source that does not exist was accepted")
	}
}
