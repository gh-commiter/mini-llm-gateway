package coreapi

import (
	"bytes"
	"context"
	"encoding/json"
	"minigw/core"
)

type restoreBody struct {
	Revision         int64      `json:"revision"`
	ExpectedRevision int64      `json:"expectedRevision"`
	Class            core.Class `json:"class"`
}

// Restore has exactly three keys, each once and with the documented spelling.
func restorePayload(r Request, b *restoreBody) error {
	if e := payload(r, b); e != nil {
		return e
	}
	invalid := core.ErrInvalid{What: "JSON body"}
	d := json.NewDecoder(bytes.NewReader(r.Body))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return invalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, e = d.Token()
		if e != nil {
			return invalid
		}
		key, ok := token.(string)
		if !ok || seen[key] || key != "revision" && key != "expectedRevision" && key != "class" {
			return invalid
		}
		seen[key] = true
		var value json.RawMessage
		if e = d.Decode(&value); e != nil || bytes.Equal(value, []byte("null")) {
			return invalid
		}
	}
	if len(seen) != 3 || b.Revision < 1 || b.ExpectedRevision < 1 || b.Class == "" {
		return invalid
	}
	return nil
}

func (a *API) noteHistoryRoute(ctx context.Context, st *core.Store, p *core.Principal, project, id int64, rest []string, r Request) Response {
	if len(rest) == 1 && rest[0] == "history" && r.Method == "GET" {
		v, e := st.ListNoteHistory(ctx, p, project, id)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	if len(rest) == 2 && rest[0] == "history" && r.Method == "GET" {
		rev, ok := posID(rest[1])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		v, e := st.GetNoteVersion(ctx, p, project, id, rev)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	if len(rest) == 1 && rest[0] == "restore" && r.Method == "POST" {
		var b restoreBody
		if e := restorePayload(r, &b); e != nil {
			if p != nil {
				if ce := st.CanChangeNote(ctx, *p, project, id); ce != nil {
					return a.errResponse(ce)
				}
			}
			return a.errResponse(e)
		}
		n, e := st.RestoreNote(ctx, p, project, id, b.Revision, b.ExpectedRevision, b.Class)
		if e != nil {
			return a.errResponse(e)
		}
		if p == nil {
			return resp(200, adminNote(n, true))
		}
		return resp(200, toNote(n, true))
	}
	return a.errResponse(core.ErrNotFound)
}
