package coreapi

import (
	"embed"
	"encoding/json"
	"testing"
)

// The Mac app (Swift) mirrors the path tests and builds the fixed bodies for a helper that is down. Both sides read
// these files in their own tests; if coreapi changes, this test fails first and the fixture is the thing to update.

//go:embed testdata
var fixtures embed.FS // embedded: the standalone test binary (Windows) runs without the source tree

func fixtureBody(t *testing.T, name string) string {
	t.Helper()
	b, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnavailableMatchesFixture(t *testing.T) {
	r := Unavailable()
	if want := fixtureBody(t, "unavailable.json"); r.Status != 503 || string(r.Body) != want {
		t.Fatalf("Unavailable() = %d %s, fixture %s", r.Status, r.Body, want)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
}

func TestNoProjectAccessMatchesFixture(t *testing.T) {
	if want := fixtureBody(t, "no_project_access.json"); NoProjectAccess().Status != 403 || string(NoProjectAccess().Body) != want {
		t.Fatalf("NoProjectAccess() = %s, fixture %s", NoProjectAccess().Body, want)
	}
}

// Admin paths match by their first segment only ("projects", "teams", ...): the rest of the path is routed (and answered 404)
// by the core, so a row such as /projects/3/dependsx is true here and a 404 from the route, not false.
func TestPathFixture(t *testing.T) {
	var rows []struct {
		Kind, Method, Path string
		Want               bool
	}
	if err := json.Unmarshal([]byte(fixtureBody(t, "paths.json")), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var got bool
		switch r.Kind {
		case "member":
			got = IsMemberPath(r.Path)
		case "admin":
			got = IsAdminPath(r.Method, r.Path)
		default:
			t.Fatalf("kind %q", r.Kind)
		}
		if got != r.Want {
			t.Errorf("%s %s %s: got %v, fixture says %v", r.Kind, r.Method, r.Path, got, r.Want)
		}
	}
}
