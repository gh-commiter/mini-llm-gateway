package core

import (
	"strings"
	"testing"
)

// The trailer matcher is exact: one complete trailer in the format trailer() writes, at the end, nothing after it. Built here with the real
// writer, so a change of the format cannot drift from the matcher.
func TestTrailerMatchIsExact(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	secret, e := f.upstreamKey(ctx)
	f.ok(e)
	p := &pusher{secret: secret, up: "hub"}
	n := UpstreamNote{ID: notes["Structure one"].ID, Author: "ana-maria"} // a hyphen in the author is written as U+2010
	trailer := p.trailer(fk, n, 0)
	sup := p.trailer(fk, n, 17)
	hash := func(body string) string {
		return upstreamHash(Note{Class: Structure, EntryKind: KindFinding, Title: "T", Body: body})
	}
	ref, author := upstreamRef(secret, "hub", fk, n.ID), trailerAuthor(n.Author)
	same := func(body, text string) int {
		return upNote{Class: Structure, Title: "T", Body: text, EntryKind: KindFinding}.sameAs(hash(body), ref, author)
	}
	for _, body := range []string{"our text", "our text\n\n", "line one\n\nline two\n"} { // incl. a body that itself ends with newlines
		if same(body, body+trailer) != 1 || same(body, body+sup) != 1 {
			t.Errorf("legitimate trailer after %q is not equal", body)
		}
	}
	fake := "<!-- minigw upstream ref 0123456789ab, author x -->\n"
	for name, text := range map[string]string{
		"text appended on the trailer's line":   "our text" + strings.TrimSuffix(trailer, "\n") + " IMPORTANT HUB EDIT <!-- extra -->\n",
		"a second comment on that line":         "our text" + strings.TrimSuffix(trailer, "\n") + "<!-- extra -->\n",
		"text after the trailer's line":         "our text" + trailer + "added later\n",
		"two line feeds after":                  "our text" + trailer + "\n",
		"a trailer of ours earlier, edited end": "our text" + trailer + "more words\n\n" + "x",
		"no trailer":                            "our text",
		"other text before the trailer":         "our text, changed" + trailer,
	} {
		if same("our text", text) == 1 {
			t.Errorf("%s: counted as our text", name)
		}
	}
	// a trailer-like comment earlier in the body is part of the text, not stripped; a well-formed trailer with other field values (another
	// reference, another author) is not ours: the text counts as changed
	quoted := "quoting " + strings.TrimSpace(fake) + " in the text"
	if same(quoted, quoted+trailer) != 1 {
		t.Error("a trailer-like comment earlier in the body was stripped")
	}
	if same("our text", "our text\n\n"+fake) == 1 || same("our text", "our text"+strings.Replace(trailer, ", author ", ", author someone else", 1)) == 1 || same("our text", "our text"+"\n\n"+fake+"more") == 1 || same("our text", "our text<!-- minigw upstream ref 0123456789ab, author x -->\n") == 1 {
		t.Error("a forged or unseparated trailer was taken for ours")
	}
	// relay protection is lenient (suspicion), the comparison exact (proof): text appended on the trailer's line is still "from a gateway", never
	// pushed onward, while the same text is not equal to what we sent
	appended := "our text" + strings.TrimSuffix(trailer, "\n") + " IMPORTANT HUB EDIT <!-- extra -->\n"
	if !fromGateway("text"+trailer) || !fromGateway(appended) || same("our text", appended) == 1 || fromGateway("plain text") {
		t.Error("fromGateway must stay lenient while sameAs stays exact")
	}
}
