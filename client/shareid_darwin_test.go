package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// #84: na tem Macu firmlink /System/Volumes/Data/Users/<ime> je dom (isti st_dev/st_ino, realpath ga pusti),
// /System/Volumes/Data je koren nosilca z domom, /Users vsebuje dom; projekt pod aliasom in drug disk
// (/Volumes/<Disk>) sta dovoljena.
func TestShareRefusalMacFirmlinks(t *testing.T) {
	home, _ := os.UserHomeDir()
	data := "/System/Volumes/Data"
	if _, err := os.Stat(data + home); err != nil {
		t.Skip("no data volume alias of home")
	}
	for _, d := range []string{data + home, data, data + "/Users", "/Users", home, "/"} {
		if shareRefusal(d) == "" {
			t.Errorf("%s allowed", d)
		}
	}
	proj := filepath.Join(data+home, "minigw-share-id-test")
	os.Mkdir(proj, 0o755)
	defer os.Remove(proj)
	if why := shareRefusal(proj); why != "" {
		t.Errorf("%s refused: %s", proj, why)
	}
	vols, _ := os.ReadDir("/Volumes")
	for _, v := range vols {
		p := filepath.Join("/Volumes", v.Name())
		if v.IsDir() && v.Name()[0] != '.' && v.Name() != "Recovery" && os.Getuid() == ownerOf(p) {
			if why := shareRefusal(p); why != "" {
				t.Errorf("%s refused: %s", p, why)
			}
		}
	}
}

func ownerOf(p string) int {
	var st syscall.Stat_t
	if syscall.Stat(p, &st) != nil {
		return -1
	}
	return int(st.Uid)
}

func TestShareAddMacFirmlink(t *testing.T) {
	_, proj := fakeHome(t)
	alias := "/System/Volumes/Data" + proj
	if _, err := os.Stat(alias); err != nil {
		t.Skip("no data volume alias")
	}
	shareCommand([]string{"add", proj})
	shareCommand([]string{"add", "--work=sync", alias})
	got := sharedFolders()
	if len(got) != 1 || got[0].Path != proj || got[0].Work != "sync" {
		t.Fatalf("shares: %+v", got)
	}
	f := sharedFoldersFile()
	if err := os.WriteFile(f, []byte(formatSharedFolders(append(got, sharedFolder{Path: alias}))), 0o600); err != nil {
		t.Fatal(err)
	}
	shareCommand([]string{"add", alias})
	if got = sharedFolders(); len(got) != 1 || got[0].Path != proj {
		t.Fatalf("duplicate aliases: %+v", got)
	}
}
