package core

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The one rule for invisible characters lets an emoji through whole (variation selector 16, zero-width joiner between emoji, keycaps,
// skin tones, flags) and keeps hiding everything else.
func TestEmojiAreVisibleTheRestStaysHidden(t *testing.T) {
	zwj, vs := "\u200d", "\ufe0f"
	for s, want := range map[string]int{
		"\u26a0" + vs + " x": 0, "\u2714" + vs: 0, "\u2139" + vs: 0, "\U0001F468" + zwj + "\U0001F4BB": 0, "\U0001F44D\U0001F3FD": 0, "1" + vs + "\u20e3": 0,
		"#" + vs + "\u20e3": 0, "\U0001F1F8\U0001F1EE": 0, "\u2764" + vs + zwj + "\U0001F525": 0, "a b": 0, "\u00a9" + vs: 0,
		"a" + zwj + "b": 1, "a" + vs + "b": 1, "a\u200bb": 1, "a\u202eb": 1, "1" + vs + "b": 1, "\U0001F468" + zwj + "b": 1, "a\u00adb": 1,
		"\U0001F3F4\U000e0067\U000e0062\U000e007f": 3, // an emoji tag sequence stays refused
		vs:                        1,
		"\u2192" + zwj + "\u2192": 1, "\u231a" + zwj + "\U0001F600": 1, "\U0001F600" + zwj + "\u231a": 1, // arrows and technical symbols are not joined
		"1" + vs:               1, // a keycap needs its U+20E3
		"\U0001F600" + vs + vs: 1, // a second selector in a row
		"\U0001F600" + strings.Repeat(zwj+"\U0001F600", 7): 0,
		"\U0001F600" + strings.Repeat(zwj+"\U0001F600", 9): 2, // at most 7 joiners in one sequence
	} {
		if _, got := dropInvisibleCount(s); got != want {
			t.Errorf("%q: %d hidden, want %d", s, got, want)
		}
	}
	if got := mirrorBody("\u26a0\ufe0f Pitfall\nbody \U0001F468\u200d\U0001F4BB"); got != "\u26a0\ufe0f Pitfall\nbody \U0001F468\u200d\U0001F4BB" {
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

func TestSubmitRefusesTabAndNewlineInATitle(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	for _, title := range []string{"A\tB", "A\nB"} {
		_, _, e := f.Submit(ctx, f.p("ana"), project, "t1", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: title, Body: "x"}})
		if e == nil || !strings.Contains(e.Error(), "the title has 1 hidden characters (line 1 column 2: U+000") {
			t.Fatalf("%q: %v", title, e)
		}
	}
	if _, _, e := f.Submit(ctx, f.p("ana"), project, "t2", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "A", Body: "x\ty\nz"}}); e != nil {
		t.Fatal(e)
	}
}

// invisibleRune over ALL code points against the compact list of ranges in testdata/invisible-ranges.json, which the Swift client's test reads
// too, so that the two rules cannot drift apart. MINIGW_REGEN_FIXTURE=1 rewrites the file from this rule (then review the diff).
func TestInvisibleRangesFixture(t *testing.T) {
	var got [][2]int
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF || !invisibleRune(r) {
			continue
		}
		if n := len(got); n > 0 && got[n-1][1] == int(r)-1 {
			got[n-1][1] = int(r)
		} else {
			got = append(got, [2]int{int(r), int(r)})
		}
	}
	const path = "../testdata/invisible-ranges.json"
	if os.Getenv("MINIGW_REGEN_FIXTURE") != "" {
		b, _ := json.Marshal(got)
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want [][2]int
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the invisible code points differ from testdata/invisible-ranges.json: %v", got)
	}
}
