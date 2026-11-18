package main

// minigw gw labels eval --file <examples.jsonl> [--show-errors]: the owner's local check of the label model (docs/usage.md).
// The file is read here, the gateway classifies (the model lives there) and answers numbers; the example text never comes back:
// the misses are listed from this process's own copy, only with --show-errors, only to this terminal. The Mac client
// (Sources/RouterCore/AdminCommands.swift) prints the same; client/testdata/admin-commands.json holds the shared cases.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"minigw/decide"
)

const (
	labelEvalMaxFile = 1 << 20 // the file; the gateway takes a body of the same size
	labelEvalShowLen = 120     // characters of an example shown for a miss
)

func labelsCommand(a *adminClient, pos []string, o coreOpts) {
	if len(pos) != 2 || pos[1] != "eval" || o.file == "" {
		fail("minigw gw labels eval --file <examples.jsonl> [--show-errors]")
	}
	f, err := os.Open(o.file)
	if err != nil {
		fail("labels eval: cannot open the file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, labelEvalMaxFile+1))
	if err != nil {
		fail("labels eval: cannot read the file")
	}
	if len(data) > labelEvalMaxFile {
		fail("labels eval: the file is larger than 1 MiB")
	}
	if !utf8.Valid(data) {
		fail("labels eval: the file is not valid UTF-8")
	}
	var examples []labelEvalExample
	var lines []int // the file line of each example
	for i, raw := range bytes.Split(data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		if len(examples) == labelEvalMaxExamples {
			fail(fmt.Sprintf("labels eval: at most %d examples", labelEvalMaxExamples))
		}
		var e labelEvalExample
		if decodeStrict(raw, &e) != nil {
			fail(fmt.Sprintf("labels eval: line %d: one JSON object with text, phase, activity", i+1))
		}
		if strings.TrimSpace(e.Text) == "" {
			fail(fmt.Sprintf("labels eval: line %d: text is required", i+1))
		}
		if e.Phase == "" && e.Activity == "" {
			fail(fmt.Sprintf("labels eval: line %d: needs a phase or an activity", i+1))
		}
		e.Text = decide.CapInput(e.Text)
		examples, lines = append(examples, e), append(lines, i+1)
	}
	if len(examples) == 0 {
		fail("labels eval: the file has no examples")
	}
	if b, _ := json.Marshal(labelEvalRequest{Examples: examples}); len(b) > labelEvalMaxFile {
		fail("labels eval: the examples are larger than the 1 MiB the gateway takes: shorten the texts")
	}
	var r struct {
		Eval labelEvalResult `json:"eval"`
	}
	a.do("POST", "/usage/labels/eval", labelEvalRequest{Examples: examples}, &r)
	fmt.Print(labelEvalText(r.Eval))
	if !o.showErrors {
		if len(r.Eval.Wrong) > 0 {
			say("misses: " + fmt.Sprint(len(r.Eval.Wrong)) + " (--show-errors lists them with their text; the text goes to this terminal only)")
		}
		return
	}
	say("misses at 2 of 3 (file line, kind, hand label -> model):")
	for _, w := range r.Eval.Wrong {
		if w.Line < 1 || w.Line > len(examples) {
			continue
		}
		got := w.Got
		if got == "" {
			got = "-"
		}
		say(clean(fmt.Sprintf("  line %d  %s  %s -> %s", lines[w.Line-1], w.Kind, w.Expected, got)))
		say("    " + labelEvalSnippet(examples[w.Line-1].Text))
	}
	if len(r.Eval.Wrong) == labelEvalMaxWrong {
		say(fmt.Sprintf("  (the first %d)", labelEvalMaxWrong))
	}
}

// labelEvalSnippet: one clean line of an example for the terminal.
func labelEvalSnippet(text string) string {
	t := strings.Join(strings.Fields(clean(text)), " ")
	if r := []rune(t); len(r) > labelEvalShowLen {
		t = string(r[:labelEvalShowLen]) + "…"
	}
	return t
}

func labelEvalPct(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", (n*100+of/2)/of)
}

// labelEvalText: the numbers, in one format for both clients.
func labelEvalText(e labelEvalResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d examples, %d run, %d not run\n", clean(e.Backend), e.Examples, e.Run, e.NotRun)
	for _, k := range []struct {
		name string
		kind labelEvalKind
	}{{"phase", e.Phase}, {"activity", e.Activity}} {
		fmt.Fprintf(&b, "%s: %d hand-labelled\n", k.name, k.kind.Scored)
		for _, c := range []struct {
			name string
			cell labelEvalCell
		}{{"3 of 3 agree", k.kind.One}, {"2 of 3 agree", k.kind.Two}} {
			fmt.Fprintf(&b, "  %s: coverage %s (%d of %d), accuracy %s (%d of %d)\n", c.name,
				labelEvalPct(c.cell.Decided, k.kind.Scored), c.cell.Decided, k.kind.Scored, labelEvalPct(c.cell.Correct, c.cell.Decided), c.cell.Correct, c.cell.Decided)
		}
		if m := e.Mistakes[k.name]; len(m) > 0 {
			fmt.Fprintf(&b, "  most frequent mistakes at 2 of 3 (hand label -> model):\n")
			for _, c := range m {
				fmt.Fprintf(&b, "    %s -> %s  %d\n", clean(c.Expected), clean(c.Got), c.N)
			}
		}
	}
	b.WriteString("This is agreement with your labels on these examples, not calibrated accuracy. Nothing was stored except one internal usage event for the model time.\n")
	return b.String()
}
