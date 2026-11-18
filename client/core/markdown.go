package core

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Markdown uses JSON string values, with no YAML interpretation.
func Markdown(n Note, key string) []byte {
	kind := n.EntryKind
	if kind == "" {
		kind = KindFinding
	}
	fields := [][2]string{{"id", i64(n.ID)}, {"title", n.Title}, {"class", string(n.Class)}, {"kind", n.Provenance.SourceKind}, {"entry_kind", kind}, {"revision", i64(n.Revision)}, {"author", n.Author}, {"created", n.Created.Format(time.RFC3339Nano)}, {"updated", n.Updated.Format(time.RFC3339Nano)}, {"hash", contentHash(n.Title, n.Body)}}
	if n.Origin != "" {
		fields = append(fields, [2]string{"origin", n.Origin})
	}
	if key != "" {
		fields = append(fields, [2]string{"import_key", key})
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, f := range fields {
		v, _ := json.Marshal(f[1])
		fmt.Fprintf(&b, "%s: %s\n", f[0], v)
	}
	b.WriteString("---\n")
	b.WriteString(n.Body)
	return []byte(b.String())
}

func parseMarkdown(b []byte) (map[string]string, string, error) {
	if !utf8.Valid(b) {
		return nil, "", ErrInvalid{"UTF-8 Markdown"}
	}
	text := string(b)
	fields := map[string]string{}
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return fields, text, nil
	}
	_, rest, _ := strings.Cut(text, "\n")
	for {
		line, tail, ok := strings.Cut(rest, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return fields, tail, nil
		}
		if !ok {
			return nil, "", ErrInvalid{"unclosed front matter"}
		}
		k, v, ok := strings.Cut(strings.TrimSuffix(line, "\r"), ":")
		if !ok || k == "" {
			return nil, "", ErrInvalid{"front matter"}
		}
		if _, exists := fields[k]; exists {
			return nil, "", ErrInvalid{"duplicate front matter field"}
		}
		var value string
		if !strings.HasPrefix(strings.TrimSpace(v), "\"") || json.Unmarshal([]byte(strings.TrimSpace(v)), &value) != nil {
			return nil, "", ErrInvalid{"front matter values must be JSON strings"}
		}
		fields[k] = value
		rest = tail
	}
}

type MarkdownInput struct {
	Markdown string `json:"markdown"`
	Path     string `json:"path"`
	Class    Class  `json:"class"`
	Submit   bool   `json:"submit"`
}
type ImportResult struct {
	Action       string `json:"action"`
	Key          string `json:"key"`
	NoteID       int64  `json:"note,omitempty"`
	SubmissionID int64  `json:"submission,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (r ImportResult) String() string {
	s := r.Action + " " + r.Key
	if r.Reason != "" {
		s += ": " + r.Reason
	}
	return s
}
func markdownInput(in MarkdownInput) (NoteInput, string, error) {
	f, body, err := parseMarkdown([]byte(in.Markdown))
	if err != nil {
		return NoteInput{}, "", err
	}
	c := in.Class
	if c == "" {
		c = Class(f["class"])
		if c == "" {
			c = Data
		}
	}
	title := f["title"]
	if title == "" {
		title = strings.TrimSuffix(path.Base(in.Path), path.Ext(in.Path))
	}
	key := "path:" + in.Path
	if id, ok := f["id"]; ok {
		key = "id:" + id
	}
	if k := f["import_key"]; strings.HasPrefix(k, "gw:") {
		key = k
	}
	note := NoteInput{Class: c, Title: title, Body: lineEndings(body), EntryKind: f["entry_kind"]}
	if err = checkInput(note); err == nil {
		err = checkSize(note)
	}
	if in.Path == "" || len(key) > 4096 {
		err = ErrInvalid{"import path or key"}
	}
	return note, key, err
}
func markdownID(f map[string]string) (int64, error) {
	id, err := strconv.ParseInt(f["id"], 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid{"note id"}
	}
	return id, nil
}
