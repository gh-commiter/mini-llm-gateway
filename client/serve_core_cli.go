package main

// minigw gw user|bind|project|member|grant|core …: the core's admin API from the terminal (serve_core_admin.go).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"minigw/core"
)

const coreHelp = `
  minigw gw user list | add <name> | rename <user> <name> | activate <user> | deactivate <user>
  minigw gw bind list | <device id> <user> | remove <device id>        which user a device belongs to
  minigw gw project list | add <slug> <name> | archive <project> | unarchive <project>
  minigw gw member <project> list | set <user> <role> | remove <user>   role: reader, developer, admin
  minigw gw grant <source> list | add <reader> <class> | remove <reader> <class>   class: structure, data
  minigw gw core backup <file>                          a consistent copy of the core database (gateway running)
  minigw gw core restore <file> [--yes]                 only with the gateway stopped
  minigw gw core audit                                  the audit trail of the core
(<user> and <project>: the name or slug, or the number.)`

type coreUser struct {
	ID     int64
	Name   string
	Active bool
	Owner  bool
}

type coreProject struct {
	ID       int64
	Slug     string
	Name     string
	Archived bool
}

// userID resolves a user given by name or number.
func userID(a *adminClient, ref string) int64 {
	var r struct{ Users []coreUser }
	a.do("GET", "/users", nil, &r)
	n, _ := strconv.ParseInt(ref, 10, 64)
	for _, u := range r.Users {
		if u.ID == n || strings.EqualFold(u.Name, ref) {
			return u.ID
		}
	}
	fail("no user " + strconv.Quote(ref) + " (minigw gw user list)")
	return 0
}

// projectID resolves a project given by slug or number.
func projectID(a *adminClient, ref string) int64 {
	var r struct{ Projects []coreProject }
	a.do("GET", "/projects", nil, &r)
	n, _ := strconv.ParseInt(ref, 10, 64)
	for _, p := range r.Projects {
		if p.ID == n || p.Slug == ref {
			return p.ID
		}
	}
	fail("no project " + strconv.Quote(ref) + " (minigw gw project list)")
	return 0
}

func coreCommand(a *adminClient, pos []string) {
	need := func(min, max int) {
		if len(pos) < min || len(pos) > max {
			fail(strings.TrimSpace(coreHelp))
		}
	}
	id := strconv.FormatInt
	switch pos[0] {
	case "user":
		need(2, 4)
		switch {
		case pos[1] == "list" && len(pos) == 2:
			var r struct{ Users []coreUser }
			a.do("GET", "/users", nil, &r)
			for _, u := range r.Users {
				state := ""
				if !u.Active {
					state = "  (deactivated)"
				}
				if u.Owner {
					state += "  (owner)"
				}
				say(clean(fmt.Sprintf("%d  %s%s", u.ID, u.Name, state)))
			}
		case pos[1] == "add" && len(pos) == 3:
			a.do("POST", "/users", map[string]any{"name": pos[2]}, nil)
			say("user " + clean(pos[2]) + " added")
		case pos[1] == "rename" && len(pos) == 4:
			a.do("PATCH", "/users/"+id(userID(a, pos[2]), 10), map[string]any{"name": pos[3]}, nil)
		case (pos[1] == "activate" || pos[1] == "deactivate") && len(pos) == 3:
			a.do("PATCH", "/users/"+id(userID(a, pos[2]), 10), map[string]any{"active": pos[1] == "activate"}, nil)
			if pos[1] == "deactivate" {
				say("deactivated: the user's runs and capabilities end (the epoch grew)")
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "bind":
		switch {
		case len(pos) == 2 && pos[1] == "list":
			var r struct {
				Bindings []struct {
					Device string
					User   int64
				}
			}
			a.do("GET", "/bindings", nil, &r)
			for _, b := range r.Bindings {
				say(fmt.Sprintf("%s  user %d", b.Device, b.User))
			}
		case len(pos) == 3 && pos[1] == "remove":
			a.do("DELETE", "/bindings/"+url.PathEscape(pos[2]), nil, nil)
		case len(pos) == 3 && validDeviceID(pos[1]):
			a.do("PUT", "/bindings/"+url.PathEscape(pos[1]), map[string]any{"user": userID(a, pos[2])}, nil)
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "project":
		need(2, 4)
		switch {
		case pos[1] == "list" && len(pos) == 2:
			var r struct{ Projects []projectJSON }
			a.do("GET", "/projects", nil, &r)
			for _, p := range r.Projects {
				arch := ""
				if p.Archived {
					arch = "  (archived)"
				}
				say(clean(fmt.Sprintf("%d  %s  %s%s", p.ID, p.Slug, p.Name, arch)))
			}
		case pos[1] == "add" && len(pos) == 4:
			a.do("POST", "/projects", map[string]any{"slug": pos[2], "name": pos[3]}, nil)
			say("project " + clean(pos[2]) + " added")
		case (pos[1] == "archive" || pos[1] == "unarchive") && len(pos) == 3:
			a.do("PATCH", "/projects/"+id(projectID(a, pos[2]), 10), map[string]any{"archived": pos[1] == "archive"}, nil)
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "member":
		need(3, 5)
		p := id(projectID(a, pos[1]), 10)
		switch {
		case pos[2] == "list" && len(pos) == 3:
			var r struct {
				Members []struct {
					User       int64
					Name, Role string
				}
			}
			a.do("GET", "/projects/"+p+"/members", nil, &r)
			for _, m := range r.Members {
				say(clean(fmt.Sprintf("%d  %s  %s", m.User, m.Name, m.Role)))
			}
		case pos[2] == "set" && len(pos) == 5:
			a.do("PUT", "/projects/"+p+"/members/"+id(userID(a, pos[3]), 10), map[string]any{"role": pos[4]}, nil)
		case pos[2] == "remove" && len(pos) == 4:
			a.do("DELETE", "/projects/"+p+"/members/"+id(userID(a, pos[3]), 10), nil, nil)
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "grant":
		need(3, 5)
		src := id(projectID(a, pos[1]), 10)
		switch {
		case pos[2] == "list" && len(pos) == 3:
			var r struct {
				Grants []struct {
					Source, Reader int64
					Class          string
				}
			}
			a.do("GET", "/grants", nil, &r)
			for _, g := range r.Grants {
				if id(g.Source, 10) == src {
					say(fmt.Sprintf("project %d reads %s of %s", g.Reader, g.Class, pos[1]))
				}
			}
		case pos[2] == "add" && len(pos) == 5:
			a.do("POST", "/grants", map[string]any{"source": src, "reader": projectID(a, pos[3]), "class": pos[4]}, nil)
		case pos[2] == "remove" && len(pos) == 5:
			a.do("DELETE", "/grants?source="+src+"&reader="+id(projectID(a, pos[3]), 10)+"&class="+url.QueryEscape(pos[4]), nil, nil)
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "core":
		need(2, 3)
		switch {
		case pos[1] == "backup" && len(pos) == 3:
			if a.off != nil {
				fail("the gateway is not running: a backup needs it (start it, or copy the core folder while it is stopped)")
			}
			file, err := filepath.Abs(pos[2])
			if err != nil {
				fail(err.Error())
			}
			a.do("POST", "/core/backup", map[string]any{"file": file}, nil)
			say("backup written to " + file)
		case pos[1] == "audit" && len(pos) == 2:
			var after int64
			for {
				var r struct {
					Events []struct {
						ID                           int64
						Time, Actor, Action, Subject string
						Detail                       any
					}
					Next int64
				}
				a.do("GET", "/audit?after="+id(after, 10)+"&limit=500", nil, &r)
				for _, e := range r.Events {
					say(clean(fmt.Sprintf("%d  %s  %s  %s  %s  %v", e.ID, e.Time, e.Actor, e.Action, e.Subject, e.Detail)))
				}
				if len(r.Events) == 0 {
					break
				}
				after = r.Next
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	}
}

// coreRestore runs core.Restore on the config folder's core, with no gateway running (the store's lock tells).
func coreRestore(cfgPath, file string, yes bool, in io.Reader) {
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	say("Restoring " + file + " brings back the users, bindings, memberships and grants of that backup, also ones revoked since.")
	say("The approved device list (serve.json) is not part of the backup and stays as it is. Every run and conversation ends (the epoch grows).")
	if !yes {
		fmt.Print("Continue? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
			fail("cancelled")
		}
	}
	err := core.Restore(filepath.Join(cfgDir, coreDirName), file)
	switch {
	case errors.Is(err, core.ErrLocked):
		fail("the gateway (or another minigw) has the core database open: stop minigw serve first, then run the restore again")
	case err != nil:
		fail("restore failed, the previous database is in place: " + err.Error())
	}
	say("restored; the previous database is kept next to it as core-<schema>-<time>.db")
	if _, err := os.Stat(filepath.Join(cfgDir, coreDirName)); err != nil {
		fail(err.Error())
	}
}
