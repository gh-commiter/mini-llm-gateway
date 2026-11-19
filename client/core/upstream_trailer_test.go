package core

import (
	"strings"
	"testing"
)

// The proof of a trailer parses nothing: the expected trailer is built from what the receipt knows and compared by string. Built here with the
// real writer, so a change of the format cannot drift from the check.
func TestOwnTrailerIsProvedByStringComparison(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	secret, e := f.upstreamKey(ctx)
	f.ok(e)
	p := &pusher{secret: secret, up: "hub"}
	const sub = 20 // the receipt's own submission id: the superseded one is some id below it
	for _, name := range []string{"ana-maria", "Ana", "Ana; supersedes submission 17", "Ana; supersedes submission 5 -- x"} {
		n := UpstreamNote{ID: notes["Structure one"].ID, Author: name} // a hyphen in the author is written as U+2010
		ref, author := upstreamRef(secret, "hub", fk, n.ID), trailerAuthor(n.Author)
		hash := func(body string) string {
			return upstreamHash(Note{Class: Structure, EntryKind: KindFinding, Title: "T", Body: body})
		}
		same := func(body, text string) int {
			return upNote{Class: Structure, Title: "T", Body: text, EntryKind: KindFinding}.sameAs(hash(body), ref, author, sub)
		}
		plain, sup, big := p.trailer(fk, n, 0), p.trailer(fk, n, 17), p.trailer(fk, n, 19)
		for _, body := range []string{"our text", "our text\n\n", "line one\n\nline two\n"} { // incl. a body that itself ends with newlines
			for _, tr := range []string{plain, sup, big} {
				if same(body, body+tr) != 1 {
					t.Errorf("%q: legitimate trailer %q after %q is not equal", name, tr, body)
				}
			}
		}
		cut := strings.TrimSuffix(plain, "\n")
		for what, text := range map[string]string{
			"text appended on the trailer's line":       "our text" + cut + " IMPORTANT HUB EDIT <!-- extra -->\n",
			"a second comment on that line":             "our text" + cut + "<!-- extra -->\n",
			"text after the trailer's line":             "our text" + plain + "added later\n",
			"two line feeds after":                      "our text" + plain + "\n",
			"no trailer":                                "our text",
			"changed text before the trailer":           "our text, changed" + plain,
			"a supersedes id at the receipt's own id":   "our text" + p.trailer(fk, n, sub),
			"a supersedes id above it":                  "our text" + p.trailer(fk, n, sub+5),
			"a carriage return":                         "our text" + strings.Replace(plain, " -->", "\r -->", 1),
			"a tab":                                     "our text" + strings.Replace(plain, " -->", "\t -->", 1),
			"a leading zero":                            "our text" + strings.Replace(sup, " 17 ", " 017 ", 1),
			"a sign":                                    "our text" + strings.Replace(sup, " 17 ", " +17 ", 1),
			"zero":                                      "our text" + strings.Replace(sup, " 17 ", " 0 ", 1),
			"an id that overflows":                      "our text" + strings.Replace(sup, " 17 ", " 99999999999999999999 ", 1),
			"another reference":                         "our text" + strings.Replace(plain, ref, "0123456789ab", 1),
			"another author":                            "our text" + strings.Replace(plain, ", author ", ", author someone else ", 1),
			"a well-formed trailer of another note":     "our text\n\n" + upstreamRefMark + "0123456789ab, author x -->\n",
			"the trailer, then another trailer's words": "our text" + plain + upstreamRefMark + "0123456789ab, author x -->\n",
		} {
			if same("our text", text) == 1 {
				t.Errorf("%q: %s: counted as our text", name, what)
			}
		}
		// a trailer-like comment earlier in the body is part of the text, not stripped
		quoted := "quoting " + strings.TrimSpace(upstreamRefMark+"0123456789ab, author x -->") + " in the text"
		if same(quoted, quoted+plain) != 1 {
			t.Errorf("%q: a trailer-like comment earlier in the body was stripped", name)
		}
		// relay protection is lenient (suspicion), the proof exact: text appended on the trailer's line is still "from a gateway", never pushed
		// onward, while the same text is not equal to what we sent
		appended := "our text" + cut + " IMPORTANT HUB EDIT <!-- extra -->\n"
		if !fromGateway("text"+plain) || !fromGateway(appended) || same("our text", appended) == 1 || fromGateway("plain text") {
			t.Errorf("%q: fromGateway must stay lenient while sameAs stays exact", name)
		}
	}
}
