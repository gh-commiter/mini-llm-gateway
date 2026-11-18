package core

import (
	"strings"
	"testing"
)

// publishedWithPendingReplacement imports a file as a proposal, publishes it with the given kind, and imports a changed copy:
// a pending replacement of the published note.
func publishedWithPendingReplacement(t *testing.T, kind string) (f *fx, pid, noteID int64, in MarkdownInput, prop ImportResult) {
	f = newFx(t)
	p := f.p("ana")
	pid = f.pid["fakture"]
	in = MarkdownInput{Markdown: "---\nentry_kind: \"" + kind + "\"\n---\npublished", Path: "k.md", Class: Structure, Submit: true}
	first, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	acc, e := f.ReviewSubmission(ctx, first.SubmissionID, true, "", f.seen(first.SubmissionID))
	f.ok(e)
	noteID = acc.NoteID
	if n, _ := f.GetNote(ctx, p, pid, noteID); n.EntryKind != kind {
		t.Fatalf("published kind %q, want %q", n.EntryKind, kind)
	}
	in.Markdown = "replacement"
	prop, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	return
}

func TestReimportOfPendingReplacementKeepsFindingKind(t *testing.T) {
	f, pid, noteID, in, prop := publishedWithPendingReplacement(t, KindFinding)
	in.Markdown = "---\nentry_kind: \"faq\"\n---\nshort answer"
	_, e := f.ImportMarkdown(ctx, f.p("ana"), pid, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, prop.SubmissionID, true, "", f.seen(prop.SubmissionID))
	f.ok(e)
	n, _ := f.GetNote(ctx, f.p("ana"), pid, noteID)
	if n.EntryKind != KindFinding {
		t.Fatalf("a published finding became %q through a re-import", n.EntryKind)
	}
}

func TestReimportOfPendingReplacementKeepsFAQKindAndLimits(t *testing.T) {
	f, pid, noteID, in, prop := publishedWithPendingReplacement(t, KindFAQ)
	in.Markdown = "---\nentry_kind: \"finding\"\n---\n" + strings.Repeat("long ", maxFAQBody)
	if _, e := f.ImportMarkdown(ctx, f.p("ana"), pid, in); e == nil {
		t.Fatal("a faq replacement above the faq size limit was accepted")
	}
	in.Markdown = "---\nentry_kind: \"finding\"\n---\nshort answer"
	_, e := f.ImportMarkdown(ctx, f.p("ana"), pid, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, prop.SubmissionID, true, "", f.seen(prop.SubmissionID))
	f.ok(e)
	n, _ := f.GetNote(ctx, f.p("ana"), pid, noteID)
	if n.EntryKind != KindFAQ {
		t.Fatalf("a published faq became %q through a re-import", n.EntryKind)
	}
}
