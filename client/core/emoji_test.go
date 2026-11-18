package core

import (
	"strings"
	"testing"
)

// The one rule for invisible characters lets an emoji through whole (variation selector 16, zero-width joiner between emoji, keycaps,
// skin tones, flags) and keeps hiding everything else.
func TestEmojiAreVisibleTheRestStaysHidden(t *testing.T) {
	for s, want := range map[string]int{
		"⚠️ x": 0, "✔️": 0, "ℹ️": 0, "👨‍💻": 0, "👍🏽": 0, "1️⃣": 0, "#️⃣": 0, "🇸🇮": 0, "❤️‍🔥": 0, "a b": 0,
		"a‍b": 1, "a️b": 1, "a​b": 1, "a‮b": 1, "1️b": 1, "👨‍b": 1, "a­b": 1,
		"🏴\U000e0067\U000e0062\U000e007f": 3, // an emoji tag sequence stays refused
		"️":                               1,
	} {
		if _, got := dropInvisibleCount(s); got != want {
			t.Errorf("%q: %d hidden, want %d", s, got, want)
		}
	}
	if got := mirrorBody("⚠️ Pitfall\nbody 👨‍💻"); got != "⚠️ Pitfall\nbody 👨‍💻" {
		t.Fatalf("%q", got)
	}
}

func dropInvisibleCount(s string) (string, int) {
	out := dropInvisible(s, -1, true)
	return out, len([]rune(s)) - len([]rune(out))
}

func TestSubmitRefusesHiddenCharactersWithPositions(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	_, _, e := f.Submit(ctx, f.p("ana"), project, "h1", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "T", Body: "one\ntwo\n0123456789abc​d‮"}})
	if e == nil || !strings.Contains(e.Error(), "the text has 2 hidden characters (line 3 column 14: U+200B ZERO WIDTH SPACE; line 3 column 16: U+202E RIGHT-TO-LEFT OVERRIDE): remove them and submit again") {
		t.Fatal(e)
	}
	if _, _, e = f.Submit(ctx, f.p("ana"), project, "h2", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "a​b", Body: "x"}}); e == nil || !strings.Contains(e.Error(), "the title has 1 hidden") {
		t.Fatal(e)
	}
}

func TestEmojiSubmissionIsAcceptedAndBOMDropped(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	v, _, e := f.Submit(ctx, f.p("ana"), project, "e1", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "⚠️ Pitfall 👨‍💻", Body: "\ufeff⚠️ body\n1️⃣ 🇸🇮 👍🏽"}})
	f.ok(e)
	r, e := f.GetSubmission(ctx, Principal{}, 0, v.ID, true)
	f.ok(e)
	if r.Hidden != 0 || r.ReviewCheck != "ok" || *r.Body != "⚠️ body\n1️⃣ 🇸🇮 👍🏽" || r.Title != "⚠️ Pitfall 👨‍💻" {
		t.Fatalf("%+v %q", r, *r.Body)
	}
	a, e := f.ReviewSubmission(ctx, v.ID, true, "", r.ReviewHash)
	f.ok(e)
	var body string
	f.ok(f.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, a.NoteID).Scan(&body))
	if body != "⚠️ body\n1️⃣ 🇸🇮 👍🏽" {
		t.Fatalf("%q", body)
	}
}
