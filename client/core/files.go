package core

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// createPrivate creates a new empty file (0600; on Windows the ACL is inherited from the protected folder).
// It fails if the file exists, and never follows a link.
func createPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeAtomic: new 0600 file in the same folder, fsync, rename; never half written.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // gone after a successful rename
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func syncDir(dir string) { // best effort: a rename is only durable after this
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// appendAudit adds one JSON line to core.audit.log, which lives outside the database so it survives a swap.
func appendAudit(dir, action string, detail map[string]any) error {
	line, err := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "action": action, "detail": detail})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, auditFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// dsn builds a file: URI that also works for Windows drive paths.
func dsn(path, query string) string {
	p := filepath.ToSlash(path)
	if p[0] != '/' {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}
