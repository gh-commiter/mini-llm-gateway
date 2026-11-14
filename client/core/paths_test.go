package core

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRelativeAndOddPaths(t *testing.T) { // core-fix 8
	names := []string{"plain", "with space", "hash#and%percent"}
	if runtime.GOOS != "windows" {
		names = append(names, "what?else", "uni-č")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			// relative folder
			s := openT(t, filepath.Join("sub", name))
			exec(t, s, `INSERT INTO meta VALUES('marker','one')`)
			if err := s.Backup(ctx, filepath.Join("bk", name+".db")); err == nil { // missing folder: an error, not a stray file
				t.Fatal("backup into a missing folder succeeded")
			}
			os.Mkdir("bk", 0o700)
			if err := s.Backup(ctx, filepath.Join("bk", name+".db")); err != nil { // relative destination
				t.Fatal(err)
			}
			s.Close()
			// the files are where the relative path says, nowhere else
			if _, err := os.Stat(filepath.Join("sub", name, dbFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(string(filepath.Separator), "sub")); err == nil {
				t.Fatal("stray absolute folder")
			}
			// relative restore source into a relative folder
			if err := Restore(filepath.Join("sub", name), filepath.Join("bk", name+".db")); err != nil {
				t.Fatal(err)
			}
			s = openT(t, filepath.Join("sub", name))
			defer s.Close()
			if marker(t, s) != "one" {
				t.Fatal("restored content")
			}
		})
	}
}
