package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"minigw/core"
	"minigw/coreapi"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxImportFiles = 10000
	maxImportDepth = 32
)

// readNoteFile opens the file once, without following a link, and reads it from that descriptor.
func readNoteFile(name string) ([]byte, error) {
	f, err := openMarker(name)
	if err != nil {
		return nil, err
	}
	return readNoteDescriptor(f)
}

func readNoteDescriptor(f *os.File) ([]byte, error) {
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", f.Name())
	}
	data, err := io.ReadAll(io.LimitReader(f, 96<<10))
	if err != nil {
		return nil, err
	}
	if len(data) >= 96<<10 {
		return nil, core.ErrTooLarge
	}
	return data, nil
}

func (e *projectEnv) kbFiles(args []string) error {
	if e.agent {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("kb", flag.ContinueOnError)
	class := fs.String("class", "", "")
	submit := fs.Bool("submit", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 3 || (pos[0] != "export" && pos[0] != "import") {
		return errors.New("usage:\n" + kbHelp)
	}
	if pos[0] == "export" && (*class != "" || *submit) {
		return errors.New("export takes no import options")
	}
	c, _ := e.connect()
	request := func(method, path string, payload []byte) ([]byte, error) {
		for {
			st, b, h := e.do(c, method, path, nil, payload)
			if st == 429 {
				seconds, _ := strconv.Atoi(h.Get("Retry-After"))
				if seconds < 1 || seconds > 60 {
					seconds = 60
				}
				e.sleep(time.Duration(seconds) * time.Second)
				continue
			}
			if st < 200 || st >= 300 {
				return nil, gatewayError(st, b, h)
			}
			return b, nil
		}
	}
	b, err := request("GET", apiProjects+"/"+url.PathEscape(pos[1]), nil)
	if err != nil {
		return err
	}
	var p struct{ ID int64 }
	if err = json.Unmarshal(b, &p); err != nil {
		return err
	}
	base := fmt.Sprintf("%s/%d", apiProjects, p.ID)
	if pos[0] == "export" {
		var notes []core.Note
		after := ""
		for {
			b, err = request("GET", base+"/notes?limit=100&after="+url.QueryEscape(after), nil)
			if err != nil {
				return err
			}
			var page struct {
				Notes []struct{ ID int64 }
				Next  string
			}
			if err = json.Unmarshal(b, &page); err != nil {
				return err
			}
			for _, entry := range page.Notes {
				raw, err := request("GET", fmt.Sprintf("%s/notes/%d", base, entry.ID), nil)
				if err != nil {
					return err
				}
				var n core.Note
				if err = json.Unmarshal(raw, &n); err != nil {
					return err
				}
				notes = append(notes, n)
			}
			after = page.Next
			if after == "" {
				break
			}
		}
		if err = os.MkdirAll(pos[2], 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(pos[2])
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("export directory must be empty")
		}
		var index strings.Builder
		index.WriteString("# " + pos[1] + "\n\n")
		write := func(name string, b []byte) error {
			f, err := os.OpenFile(filepath.Join(pos[2], name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			if err = protectDir(filepath.Join(pos[2], name)); err == nil {
				_, err = f.Write(b)
			}
			closeErr := f.Close()
			if err != nil {
				return err
			}
			return closeErr
		}
		for _, n := range notes {
			name := fmt.Sprintf("%d.md", n.ID)
			if err = write(name, core.Markdown(n, "")); err != nil {
				return err
			}
			fmt.Fprintf(&index, "- [%s](%s)\n", strings.ReplaceAll(n.Title, "\n", " "), name)
		}
		if err = write("INDEX.md", []byte(index.String())); err != nil {
			return err
		}
		e.say(fmt.Sprintf("export %d notes", len(notes)))
		return nil
	}
	root := pos[2]
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	files := []string{}
	var folder *os.Root
	if fi.IsDir() {
		folder, err = os.OpenRoot(root)
		if err != nil {
			return err
		}
		defer folder.Close()
		files, err = collectNoteFiles(folder)
		if err != nil {
			return err
		}
	} else if fi.Mode().IsRegular() && strings.EqualFold(filepath.Ext(root), ".md") {
		files = append(files, root)
	} else {
		return errors.New("import requires a Markdown file or directory")
	}
	for _, file := range files {
		rel := filepath.Base(file)
		var data []byte
		if folder != nil {
			rel = file
			var f *os.File
			f, err = openRootNote(folder, file)
			if err == nil {
				data, err = readNoteDescriptor(f)
			}
		} else {
			data, err = readNoteFile(file)
		}
		if err != nil {
			return err
		}
		if !utf8.Valid(data) {
			return errors.New("the note must be text (UTF-8)")
		}
		payload, err := json.Marshal(core.MarkdownInput{Markdown: string(data), Path: filepath.ToSlash(rel), Class: core.Class(*class), Submit: *submit})
		if err != nil {
			return err
		}
		if len(payload) > coreapi.MaxBody {
			return core.ErrTooLarge
		}
		b, err = request("POST", base+"/kb/import", payload)
		if err != nil {
			return err
		}
		var result core.ImportResult
		if err = json.Unmarshal(b, &result); err != nil {
			return err
		}
		e.say(clean(result.String()))
	}
	return nil
}

// Read directories in batches, bounding all entries before retaining file names.
func collectNoteFiles(root *os.Root) ([]string, error) {
	var files []string
	visited := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > maxImportDepth {
			return core.ErrTooLarge
		}
		f, err := root.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			entries, err := f.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			for _, d := range entries {
				visited++
				if visited > maxImportFiles {
					return core.ErrTooLarge
				}
				if depth+1 > maxImportDepth {
					return core.ErrTooLarge
				}
				name := filepath.Join(dir, d.Name())
				if d.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("not a regular file: %s", name)
				}
				if d.IsDir() {
					if err := walk(name, depth+1); err != nil {
						return err
					}
				} else if !strings.EqualFold(d.Name(), "INDEX.md") && strings.EqualFold(filepath.Ext(name), ".md") {
					if !d.Type().IsRegular() {
						return fmt.Errorf("not a regular file: %s", name)
					}
					files = append(files, name)
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	}
	err := walk(".", 0)
	sort.Strings(files)
	return files, err
}

func bundleCommand(args []string) {
	cfg, route, body, err := bundleRequest(args)
	if err != nil {
		fail(err.Error())
		return
	}
	a := openAdmin(cfg)
	var report core.BundleReport
	a.do("POST", route, body, &report)
	for _, line := range report.Lines {
		say(clean(line))
	}
}

func bundleRequest(args []string) (string, string, any, error) {
	fs := flag.NewFlagSet("gw bundle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("config", defaultServeConfigPath(), "")
	team := fs.String("team", "", "")
	source := fs.String("source", "", "")
	private := fs.Bool("with-private", false, "")
	dry := fs.Bool("dry-run", false, "")
	projects := []string{}
	mapping := map[string]string{}
	fs.Func("project", "", func(s string) error { projects = append(projects, s); return nil })
	fs.Func("map", "", func(s string) error {
		old, name, ok := strings.Cut(s, "=")
		if !ok || old == "" || name == "" {
			return errors.New("--map old=new")
		}
		if _, ok = mapping[old]; ok {
			return errors.New("duplicate map")
		}
		mapping[old] = name
		return nil
	})
	pos, err := parseFlags(fs, args)
	usage := errors.New(coreHelp)
	if err != nil || len(pos) != 2 {
		return "", "", nil, usage
	}
	file, err := filepath.Abs(pos[1])
	if err != nil {
		return "", "", nil, err
	}
	var body any
	switch pos[0] {
	case "export":
		if *team == "" || *source != "" || *dry || len(mapping) > 0 {
			return "", "", nil, usage
		}
		body = core.BundleExport{Team: *team, Projects: projects, WithPrivate: *private, Path: file}
	case "import":
		if *source == "" || *team != "" || len(projects) > 0 || *private {
			return "", "", nil, usage
		}
		body = core.BundleImport{Path: file, Source: *source, DryRun: *dry, Map: mapping}
	default:
		return "", "", nil, usage
	}
	return *cfg, "/" + pos[0], body, nil
}
