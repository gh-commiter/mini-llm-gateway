package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"minigw/core"
	"strconv"
	"strings"
	"time"
)

const historyTail = "some revisions are not listed: not kept (from before 0.8.6, or dropped from a long series of edits) or not readable by you"
const restoreNeedsCurrent = "not restored: without a terminal, name the current revision: --current <n>"

// historyTime: a time of these commands, to the minute, always with its zone: UTC, or the offset from it.
func historyTime(t time.Time) string {
	t = t.Local()
	if _, off := t.Zone(); off == 0 {
		return t.Format("2006-01-02 15:04") + " UTC"
	}
	return t.Format("2006-01-02 15:04 -07:00")
}

// versionWho: written <time> by <who>  replaced <time> by <who>. Written is when this text became the note's text,
// replaced is when something else did; the names are the ones the gateway gave (never the same fact twice).
func versionWho(v core.NoteVersion) string {
	return fmt.Sprintf("written %s by %s  replaced %s by %s", historyTime(v.Updated), orUnknown(v.WrittenBy, "unknown"), historyTime(v.Saved), orUnknown(v.ReplacedBy, "a review or import"))
}

func orUnknown(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

// Both command families use the same rendering and confirmation rules.
func noteHistoryCommand(sub, path string, id, revision int64, hasRevision bool, current int64, hasCurrent bool, class string, asJSON, terminal bool, in io.Reader, out io.Writer, request func(string, string, []byte) ([]byte, error)) error {
	say := func(s string) { fmt.Fprintln(out, clean(s)) }
	if hasRevision && revision < 1 || sub == "restore" && !hasRevision {
		return errors.New("--revision must be positive")
	}
	if hasCurrent && current < 1 {
		return errors.New("--current must be positive")
	}
	if sub == "history" && !hasRevision {
		data, err := request("GET", path+"/history", nil)
		if err != nil {
			return err
		}
		if asJSON {
			say(jsonText(strings.TrimSpace(string(data))))
			return nil
		}
		var h core.NoteHistory
		if err = json.Unmarshal(data, &h); err != nil {
			return err
		}
		say(fmt.Sprintf("note %d  revision %d (current)  %s  %s", h.Note.ID, h.Note.Revision, h.Note.Class, oneLine(h.Note.Title)))
		for _, v := range h.History {
			say(fmt.Sprintf("revision %d  %s  %s  %s  %d bytes  %s", v.Revision, versionWho(v), v.Class, v.EntryKind, v.Bytes, oneLine(v.Title)))
		}
		if len(h.History) == 0 {
			say("no saved revisions you may read")
		}
		// Every saved revision lies below the current one: the list is complete when it holds all of 1..current-1.
		if int64(len(h.History)) != h.Note.Revision-1 {
			say(historyTail)
		}
		return nil
	}
	data, err := request("GET", path, nil)
	if err != nil {
		return err
	}
	var cur noteView
	if err = json.Unmarshal(data, &cur); err != nil {
		return err
	}
	if revision == cur.Revision {
		switch {
		case sub == "restore":
			return fmt.Errorf("revision %d is the current text: nothing to restore", revision)
		case strings.HasPrefix(path, "/kb/"):
			return fmt.Errorf("revision %d is the current text, not a saved version", revision)
		}
		return fmt.Errorf("revision %d is the current text: minigw note read %d", revision, id)
	}
	data, err = request("GET", path+"/history/"+strconv.FormatInt(revision, 10), nil)
	if err != nil {
		return err
	}
	if sub == "history" && asJSON {
		say(jsonText(strings.TrimSpace(string(data))))
		return nil
	}
	var v core.NoteVersion
	if err = json.Unmarshal(data, &v); err != nil {
		return err
	}
	if sub == "history" {
		say(fmt.Sprintf("note %d  revision %d of %d  %s  %s  %s", id, v.Revision, cur.Revision, v.Class, v.EntryKind, versionWho(v)))
		say(oneLine(v.Title))
		body := ""
		if v.Body != nil {
			body = *v.Body
		}
		fmt.Fprint(out, clean(strings.ReplaceAll(body, "\t", "    ")))
		if !strings.HasSuffix(body, "\n") {
			say("")
		}
		return nil
	}
	target := class
	if target == "" {
		target = cur.Class
	}
	if target != cur.Class && target != string(v.Class) {
		return errors.New("class must be the current or saved class")
	}
	expected := cur.Revision
	if hasCurrent {
		expected = current
	}
	// The plan is printed where it is a question (before the prompt, or before the refusal that names the current
	// revision) and with the success: never before a request that is refused, so it cannot read as if it happened.
	plan := func() {
		say(fmt.Sprintf("restore note %d to the text of revision %d (now revision %d, becomes %d)", id, revision, cur.Revision, cur.Revision+1))
		if target == cur.Class {
			say("class: " + target + " (kept)")
		} else {
			say("class: " + cur.Class + " -> " + target)
		}
	}
	if !hasCurrent {
		plan()
		if !terminal {
			return errors.New(restoreNeedsCurrent)
		}
		fmt.Fprint(out, "Restore? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not restored")
		}
	}
	payload, _ := json.Marshal(struct {
		Revision         int64  `json:"revision"`
		ExpectedRevision int64  `json:"expectedRevision"`
		Class            string `json:"class"`
	}{revision, expected, target})
	data, err = request("POST", path+"/restore", payload)
	var g gwStatusError
	if errors.As(err, &g) && g.status == 403 && err.Error() == forbiddenRoleText {
		return errors.New("not allowed: restoring a saved version needs the project admin role (or the gateway owner)")
	}
	if err != nil {
		return err
	}
	if hasCurrent {
		plan()
	}
	var n noteView
	if err = json.Unmarshal(data, &n); err != nil {
		return err
	}
	line := fmt.Sprintf("restored revision %d as revision %d; the note is %s", revision, n.Revision, n.Class)
	switch {
	case target != cur.Class:
		line += fmt.Sprintf(" (class changed from %s to %s)", cur.Class, target)
	case string(v.Class) != cur.Class:
		line += fmt.Sprintf(" (current class kept; use --class %s to restore the saved class)", v.Class)
	}
	say(line)
	return nil
}
