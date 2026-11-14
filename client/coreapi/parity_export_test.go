package coreapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Mac app's Swift tests send the same table through the helper process and compare the answers with these. The
// expected answers are made here, in-process, from the same seeded store: nothing is copied by hand. With MG_PARITY_OUT
// set the store and the table are left in that folder (the Swift test sets it); otherwise a temporary one is used.
func TestParityTableIsServedInProcess(t *testing.T) {
	dir := os.Getenv("MG_PARITY_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	f := newFixtureIn(t, filepath.Join(dir, "store"))
	type row struct {
		Name     string   `json:"name"`
		Context  Context  `json:"context"`
		Request  Request  `json:"request"`
		Response Response `json:"response"`
	}
	var rows []row
	for _, c := range f.cases() {
		r := f.api.Handle(bg, c.c, c.r)
		if r.Status != c.status {
			t.Errorf("%s: %d, want %d", c.name, r.Status, c.status)
		}
		rows = append(rows, row{c.name, c.c, c.r, r})
	}
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cases.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.st.Close() // the helper opens the same folder next
}
