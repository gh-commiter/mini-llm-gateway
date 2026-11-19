package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Version skew: a gateway from before this release defaults a missing `remote` to the LOCAL name and has no destination guard, so a command
// that omitted it would silently retarget an existing link. The CLI therefore always sends `remote`: the destination of the existing link (read
// first), else the local name. Checked over every `link` case of the shared fixture, with an old gateway's reading of the body.
func TestLinkCommandNeverSendsAMissingDestination(t *testing.T) {
	var fx struct {
		Cases []*admCase `json:"cases"`
	}
	readFixture(t, "upstream-commands.json", &fx)
	seen := 0
	for _, c := range fx.Cases {
		if len(c.Args) < 2 || c.Args[1] != "link" || c.Want == nil {
			continue
		}
		for _, r := range c.Want.Requests {
			if r[0] != "PUT" {
				continue
			}
			seen++
			var body map[string]any
			if json.Unmarshal([]byte(r[2]), &body) != nil {
				t.Fatalf("%s: %s", c.Name, r[2])
			}
			remote, ok := body["remote"].(string)
			if !ok || remote == "" {
				t.Errorf("%s: an old gateway would use the local name: %s", c.Name, r[2])
			}
			if c.Name == "link auto on an existing link keeps its destination" && remote != "shared" {
				t.Errorf("%s: the existing destination is not sent: %s", c.Name, r[2])
			}
			if strings.HasPrefix(c.Name, "link ") && !strings.Contains(c.Name, "existing") && c.Opts.RemoteProject == "" && c.Opts.RemoteTeam == "" && body["team"] == false && remote != "shop" {
				t.Errorf("%s: a first link uses the local name: %s", c.Name, r[2])
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d link requests", seen)
	}
}
