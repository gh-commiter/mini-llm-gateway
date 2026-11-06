package main

// Minimalni WebDAV nad eno mapo (kot RouterCore/WebDAV.swift): router na Mac miniju mapo priklopi z
// mount_webdav, zahteve pa prek povezave minigw prispejo sem. Nič zunaj root: brez »..«, simbolne povezave
// se razrešijo in preverijo.

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type davResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

func dav(status int, headers map[string]string, body []byte) davResponse {
	if headers == nil {
		headers = map[string]string{}
	}
	if body == nil {
		body = []byte{}
	}
	return davResponse{Status: status, Headers: headers, Body: body}
}

func davHandle(method, path string, headers map[string]string, body []byte, root, prefix string) davResponse {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return dav(403, nil, nil)
	}
	root = filepath.Clean(r)
	p, rel, ok := davResolve(path, prefix, root)
	if !ok {
		return dav(403, nil, nil)
	}
	junk := isJunk(p)
	info, statErr := os.Stat(p)
	exists := !junk && statErr == nil
	isDir := exists && info.IsDir()

	switch method {
	case "OPTIONS":
		return dav(200, map[string]string{"DAV": "1, 2", "MS-Author-Via": "DAV",
			"Allow": "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, MOVE, COPY, PROPFIND, PROPPATCH, LOCK, UNLOCK"}, nil)

	case "PROPFIND":
		if !exists {
			return dav(404, nil, nil)
		}
		type item struct {
			path string
			rel  []string
		}
		items := []item{{p, rel}}
		if isDir && headers["depth"] != "0" {
			entries, _ := os.ReadDir(p)
			names := []string{}
			for _, e := range entries {
				if !isJunk(e.Name()) {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			for _, n := range names {
				items = append(items, item{filepath.Join(p, n), append(append([]string{}, rel...), n)})
			}
		}
		var sb strings.Builder
		sb.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\">")
		for _, it := range items {
			sb.WriteString(propResponse(it.path, href(prefix, it.rel)))
		}
		sb.WriteString("</D:multistatus>")
		return dav(207, map[string]string{"Content-Type": "application/xml; charset=utf-8"}, []byte(sb.String()))

	case "PROPPATCH":
		if !exists {
			return dav(404, nil, nil)
		}
		return dav(207, map[string]string{"Content-Type": "application/xml; charset=utf-8"}, []byte(
			"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\"><D:response><D:href>"+href(prefix, rel)+
				"</D:href><D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>"))

	case "GET", "HEAD":
		if !exists {
			return dav(404, nil, nil)
		}
		if isDir {
			return dav(200, map[string]string{"Content-Type": "text/plain"}, nil)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return dav(403, nil, nil)
		}
		h := map[string]string{"Content-Type": "application/octet-stream", "Last-Modified": httpDate(info.ModTime()), "Accept-Ranges": "bytes"}
		part, status := data, 200
		if rg, ok := headers["range"]; ok {
			if a, b, ok := parseRange(rg, len(data)); ok {
				part = data[a : b+1]
				h["Content-Range"] = fmt.Sprintf("bytes %d-%d/%d", a, b, len(data))
				status = 206
			}
		}
		if method == "HEAD" {
			h["Content-Length"] = strconv.Itoa(len(part))
			part = nil
		}
		return dav(status, h, part)

	case "PUT":
		if junk {
			return dav(201, nil, nil)
		}
		if isDir {
			return dav(405, nil, nil)
		}
		if !parentExists(p) {
			return dav(409, nil, nil)
		}
		if err := writeAtomic(p, body); err != nil {
			return dav(403, nil, nil)
		}
		if exists {
			return dav(204, nil, nil)
		}
		return dav(201, nil, nil)

	case "MKCOL":
		if _, err := os.Stat(p); err == nil {
			return dav(405, nil, nil)
		}
		if !parentExists(p) {
			return dav(409, nil, nil)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return dav(403, nil, nil)
		}
		return dav(201, nil, nil)

	case "DELETE":
		if junk {
			return dav(204, nil, nil)
		}
		if !exists {
			return dav(404, nil, nil)
		}
		if len(rel) == 0 {
			return dav(403, nil, nil)
		}
		if err := os.RemoveAll(p); err != nil {
			return dav(403, nil, nil)
		}
		return dav(204, nil, nil)

	case "MOVE", "COPY":
		if !exists || len(rel) == 0 {
			if junk {
				return dav(204, nil, nil)
			}
			return dav(404, nil, nil)
		}
		raw, ok := headers["destination"]
		if !ok {
			return dav(403, nil, nil)
		}
		destPath := raw
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			destPath = u.EscapedPath()
		}
		dest, destRel, ok := davResolve(destPath, prefix, root)
		if !ok || len(destRel) == 0 {
			return dav(403, nil, nil)
		}
		if isJunk(dest) {
			return dav(201, nil, nil)
		}
		if !parentExists(dest) {
			return dav(409, nil, nil)
		}
		_, derr := os.Stat(dest)
		replaced := derr == nil
		if replaced {
			if strings.ToUpper(headers["overwrite"]) == "F" {
				return dav(412, nil, nil)
			}
			os.RemoveAll(dest)
		}
		var err error
		if method == "MOVE" {
			err = os.Rename(p, dest)
		} else {
			err = copyTree(p, dest)
		}
		if err != nil {
			return dav(403, nil, nil)
		}
		if replaced {
			return dav(204, nil, nil)
		}
		return dav(201, nil, nil)

	case "LOCK":
		// Navidezen izključni zaklep: macOS webdavfs ga potrebuje za pisanje.
		created := false
		if !exists && !junk {
			if !parentExists(p) {
				return dav(409, nil, nil)
			}
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
				f.Close()
				created = true
			}
		}
		token := lockTokenRe.FindString(headers["if"])
		if token == "" {
			token = "opaquelocktoken:" + newUUID()
		}
		xml := "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:prop xmlns:D=\"DAV:\"><D:lockdiscovery><D:activelock>" +
			"<D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope><D:depth>0</D:depth>" +
			"<D:timeout>Second-3600</D:timeout><D:locktoken><D:href>" + token + "</D:href></D:locktoken>" +
			"</D:activelock></D:lockdiscovery></D:prop>"
		status := 200
		if created {
			status = 201
		}
		return dav(status, map[string]string{"Lock-Token": "<" + token + ">", "Content-Type": "application/xml; charset=utf-8"}, []byte(xml))

	case "UNLOCK":
		return dav(204, nil, nil)
	}
	return dav(405, nil, nil)
}

var lockTokenRe = regexp.MustCompile(`opaquelocktoken:[^>]+`)

// davResolve: pot zahteve (s prefix, kodirana) → datoteka pod root in relativne komponente; false, če bi ušla iz root.
func davResolve(path, prefix, root string) (string, []string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", nil, false
	}
	var comps []string
	for _, c := range strings.Split(path[len(prefix):], "/") {
		if c == "" {
			continue
		}
		name, err := url.PathUnescape(c)
		if err != nil || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") ||
			(runtime.GOOS == "windows" && strings.ContainsAny(name, `\:`)) {
			return "", nil, false
		}
		comps = append(comps, name)
	}
	full := filepath.Join(append([]string{root}, comps...)...)
	// Simbolne povezave: obstoječi del poti mora tudi po razrešitvi ostati v root.
	existing := full
	for {
		if _, err := os.Lstat(existing); err == nil || samePath(existing, root) {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", nil, false
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", nil, false
	}
	if !samePath(real, root) && !hasPathPrefix(real, root) {
		return "", nil, false
	}
	return full, comps, true
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func hasPathPrefix(p, root string) bool {
	p, root = filepath.Clean(p), filepath.Clean(root)+string(filepath.Separator)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.HasPrefix(strings.ToLower(p), strings.ToLower(root))
	}
	return strings.HasPrefix(p, root)
}

func isJunk(p string) bool {
	n := filepath.Base(p)
	return strings.HasPrefix(n, "._") || n == ".DS_Store"
}

func parentExists(p string) bool {
	info, err := os.Stat(filepath.Dir(p))
	return err == nil && info.IsDir()
}

func writeAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".minigw-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// href: kot urlPathAllowed brez »/?#[]@!$&'()*+,;=« (enako kot Swift).
func href(prefix string, comps []string) string {
	enc := make([]string, len(comps))
	for i, c := range comps {
		var b strings.Builder
		for _, ch := range []byte(c) {
			if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("-._~:", ch) >= 0 {
				b.WriteByte(ch)
			} else {
				fmt.Fprintf(&b, "%%%02X", ch)
			}
		}
		enc[i] = b.String()
	}
	return prefix + "/" + strings.Join(enc, "/")
}

func propResponse(p, href string) string {
	info, err := os.Stat(p)
	dir, size, mod := false, int64(0), time.Now()
	if err == nil {
		dir, size, mod = info.IsDir(), info.Size(), info.ModTime()
	}
	props := "<D:resourcetype>"
	if dir {
		props += "<D:collection/>"
	}
	props += "</D:resourcetype><D:getlastmodified>" + httpDate(mod) + "</D:getlastmodified>" +
		"<D:creationdate>" + mod.UTC().Format(time.RFC3339) + "</D:creationdate>"
	if !dir {
		props += fmt.Sprintf("<D:getcontentlength>%d</D:getcontentlength><D:getcontenttype>application/octet-stream</D:getcontenttype>", size)
	}
	if dir && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	return "<D:response><D:href>" + href + "</D:href><D:propstat><D:prop>" + props + "</D:prop>" +
		"<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"
}

func httpDate(t time.Time) string { return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }

// parseRange: »bytes=a-b« / »bytes=a-« / »bytes=-n« → zaprt interval; false = celotna datoteka.
func parseRange(h string, size int) (int, int, bool) {
	if size <= 0 || !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return 0, 0, false
	}
	p := strings.SplitN(h[6:], "-", 2)
	if len(p) != 2 {
		return 0, 0, false
	}
	if p[0] == "" {
		n, err := strconv.Atoi(p[1])
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		a := size - n
		if a < 0 {
			a = 0
		}
		return a, size - 1, true
	}
	a, err := strconv.Atoi(p[0])
	if err != nil || a >= size {
		return 0, 0, false
	}
	b := size - 1
	if p[1] != "" {
		if v, err := strconv.Atoi(p[1]); err == nil && v < b {
			b = v
		}
	}
	return a, b, a <= b
}
