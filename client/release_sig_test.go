package main

import (
	"strings"
	"testing"
)

// #8: podpis SHA256SUMS (ssh-keygen -Y sign) s testnim ključem (ne ključem izdaj).
const (
	testSums   = "abc  minigw-linux-amd64\n"
	testSigner = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPncnLFYhyI+yi6YSEQAkfHs146x2My1tTEWoIYOpu7c"
	testSig    = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg+dycsViHIj7KLphIRACR8ezXjr
HYzLW1MRaghg6m7twAAAAObWluaWd3LXJlbGVhc2UAAAAAAAAABnNoYTUxMgAAAFMAAAAL
c3NoLWVkMjU1MTkAAABA36WpYjKKS0aHYHuQbBryeKWYfpV8XHvMbiOGo7WAFdQV85OlRy
VvilyRC2sK4qQLIKWl1ZyHwCm5p+7OpAMVBg==
-----END SSH SIGNATURE-----`
	testOtherNSig = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg+dycsViHIj7KLphIRACR8ezXjr
HYzLW1MRaghg6m7twAAAAFb3RoZXIAAAAAAAAABnNoYTUxMgAAAFMAAAALc3NoLWVkMjU1
MTkAAABAzGGo3znpMkKjzPSRwSEVDTH8CKHTgAghc5SRidDSLplLN06YzZ9k/MYHbITtyy
Df38Qik452h9qfv1kt/YfRBg==
-----END SSH SIGNATURE-----`
)

func TestVerifySSHSig(t *testing.T) {
	keys := []string{testSigner}
	if err := verifySSHSig([]byte(testSums), []byte(testSig), keys, "minigw-release"); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	if err := verifySSHSig([]byte(strings.Replace(testSums, "abc", "abd", 1)), []byte(testSig), keys, "minigw-release"); err == nil {
		t.Fatal("tampered sums accepted")
	}
	if err := verifySSHSig([]byte(testSums), []byte(testOtherNSig), keys, "minigw-release"); err == nil {
		t.Fatal("other namespace accepted")
	}
	if err := verifySSHSig([]byte(testSums), []byte(testSig), releaseKeys, "minigw-release"); err == nil {
		t.Fatal("foreign key accepted")
	}
	if err := verifySSHSig([]byte(testSums), []byte("garbage"), keys, "minigw-release"); err == nil {
		t.Fatal("garbage accepted")
	}
	if verifyReleaseSums([]byte(testSums), []byte(testSig)) == nil {
		t.Fatal("release check accepted a test key")
	}
}
