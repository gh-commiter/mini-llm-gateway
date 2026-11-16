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

func TestRunNoteReadAuthorizationChanges(t *testing.T) {
	for _, change := range []string{"approval", "binding", "epoch", "membership", "grant", "missing project"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			epoch, _ := f.st.Epoch(bg)
			cap := core.RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: epoch}
			cx := Context{Kind: KindRun, Run: &cap, Approved: true}
			req := Request{Method: "GET", Path: fmt.Sprintf("/minigw/v1/notes/%d", f.nid["structure"]), RawQuery: "project=999&class=private"}
			if v := f.api.Handle(bg, cx, req); v.Status != 200 {
				t.Fatalf("%d %s", v.Status, v.Body)
			}
			for _, id := range []int64{f.nid["private"], f.nid["data"], 9999} {
				r := get(fmt.Sprintf("/minigw/v1/notes/%d", id))
				v := f.api.Handle(bg, cx, r)
				if v.Status != 404 || string(v.Body) != string(f.api.errResponse(core.ErrNotFound).Body) {
					t.Fatalf("hidden metadata: %d %s", v.Status, v.Body)
				}
			}
			var err error
			switch change {
			case "approval":
				cx.Approved = false
			case "binding":
				err = f.st.SetBinding(bg, "dev-cene", f.uid["ana"])
			case "epoch":
				_, err = f.st.BumpEpoch(bg, "test")
			case "membership":
				err = f.st.RemoveMember(bg, f.pid["eureko"], f.uid["cene"])
			case "grant":
				err = f.st.RemoveGrant(bg, f.pid["fakture"], f.pid["eureko"], core.Structure)
			case "missing project":
				cap.ProjectID = 0
			}
			if err != nil {
				t.Fatal(err)
			}
			if v := f.api.Handle(bg, cx, req); v.Status != 403 {
				t.Fatalf("%d %s", v.Status, v.Body)
			}
		})
	}
}
