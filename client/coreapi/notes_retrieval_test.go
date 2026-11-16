package coreapi

import (
	"context"
	"fmt"
	"testing"

	"minigw/core"
)

func TestNoteOperationsRecheckAfterAdmission(t *testing.T) {
	for _, method := range []string{"GET", "PUT", "DELETE", "POST"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			project, id := f.pid["fakture"], f.nid["structure"]
			path := fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", project, id)
			r := Request{Method: method, Path: path, Body: []byte("{")}
			if method == "POST" {
				r.Path = fmt.Sprintf("/minigw/v1/projects/%d/notes", project)
				r.Body = []byte(`{"class":"structure","title":"t-structure","body":"body"}`)
				r.Header = map[string][]string{"Idempotency-Key": {"k-structure"}}
			}
			f.api.o.AfterAdmission = func(ctx context.Context, _ Context, _ Request) {
				// The principal was admitted at the old epoch; reclassify before the operation's transaction.
				_, err := f.st.UpdateNote(ctx, core.Principal{UserID: f.uid["fani"], DeviceID: "dev-fani"}, project, id, 1,
					core.NoteInput{Class: core.Private, Title: "restricted", Body: "restricted"})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Ana still owns the private note, but may not replay its old shared representation.
			user := "dev-eva"
			if method == "POST" {
				user = "dev-ana"
			}
			got := f.api.Handle(bg, member(user), r)
			missing := f.api.errResponse(core.ErrNotFound)
			if got.Status != missing.Status || string(got.Body) != string(missing.Body) {
				t.Fatalf("%d %s, want missing response %s", got.Status, got.Body, missing.Body)
			}
		})
	}
}
