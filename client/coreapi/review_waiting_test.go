package coreapi

import (
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

// GET /projects/{id}/kb/review carries "waiting" for a reviewer; a caller who may not review gets the same 404 as for a project
// without submissions (nothing about whether any exist).
func TestReviewListWaiting(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	list := get(fmt.Sprintf("/minigw/v1/projects/%d/kb/review", pid))
	nothing := get("/minigw/v1/projects/987654/kb/review")
	answer := func(dev string, req Request) (int, string) {
		r := f.api.Handle(bg, member(dev), req)
		return r.Status, string(r.Body)
	}
	if r := f.api.Handle(bg, member("dev-fani"), list); r.Status != 200 || !strings.Contains(string(r.Body), `"waiting":0`) {
		t.Fatal(r.Status, string(r.Body))
	}
	var before [2]string
	_, before[0] = answer("dev-ana", list)
	_, before[1] = answer("dev-ana", nothing)
	if before[0] != before[1] {
		t.Fatal("a project the caller cannot review differs from one without submissions", before)
	}
	v, _, err := f.st.Submit(bg, core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}, pid, "w1", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: "Waits", Body: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := f.api.Handle(bg, member("dev-fani"), list); r.Status != 200 || !strings.Contains(string(r.Body), `"waiting":1`) {
		t.Fatal(r.Status, string(r.Body))
	}
	for _, dev := range []string{"dev-ana", "dev-eva", "dev-cene"} {
		if st, body := answer(dev, list); st != 404 || body != before[0] || strings.Contains(body, "waiting") {
			t.Fatal(dev, st, body)
		}
	}
	accept := Request{Method: "POST", Path: fmt.Sprintf("/minigw/v1/projects/0/kb/review/%d/accept", v.ID), Body: []byte(`{"reason":"","reviewHash":"` + seenHash(f.st, v.ID) + `"}`)}
	if r := f.api.Handle(bg, member("dev-fani"), accept); r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	if r := f.api.Handle(bg, member("dev-fani"), list); r.Status != 200 || !strings.Contains(string(r.Body), `"waiting":0`) {
		t.Fatal("accepted must not count", r.Status, string(r.Body))
	}
}
