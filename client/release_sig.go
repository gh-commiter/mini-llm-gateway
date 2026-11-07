package main

// #8: SHA256SUMS izdaje je podpisan (ssh-keygen -Y sign -n minigw-release, ključ ed25519 zunaj GitHuba). Ključ je
// tu pripet: ogrožen račun ali izdaja na GitHubu ne more podpisati drugačnih vsot. Preverjanje formata SSHSIG
// (PROTOCOL.sshsig) le s standardno knjižnico.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
)

// releaseKeys: pripeti javni ključi za podpis izdaj (seznam zaradi menjave ključa).
var releaseKeys = []string{
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH", // Mini Gateway release
}

const releaseNamespace = "minigw-release"

// sshString: niz žičnega formata SSH (uint32 dolžina + bajti); ostanek.
func sshString(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errors.New("short")
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(len(b)-4) < uint64(n) {
		return nil, nil, errors.New("short")
	}
	return b[4 : 4+n], b[4+n:], nil
}

func putSSHString(buf *bytes.Buffer, s []byte) {
	binary.Write(buf, binary.BigEndian, uint32(len(s)))
	buf.Write(s)
}

// ed25519Key: 32 bajtov javnega ključa iz bloba »ssh-ed25519«.
func ed25519Key(blob []byte) (ed25519.PublicKey, error) {
	typ, rest, err := sshString(blob)
	if err != nil || string(typ) != "ssh-ed25519" {
		return nil, errors.New("not an ssh-ed25519 key")
	}
	k, rest, err := sshString(rest)
	if err != nil || len(k) != ed25519.PublicKeySize || len(rest) != 0 {
		return nil, errors.New("bad ssh-ed25519 key")
	}
	return ed25519.PublicKey(k), nil
}

// verifySSHSig: podpis (oklep SSH SIGNATURE) sporočila z enim od ključev `keys` (»ssh-ed25519 AAAA…«) v imenskem
// prostoru `namespace`.
func verifySSHSig(message, armored []byte, keys []string, namespace string) error {
	s := strings.TrimSpace(string(armored))
	s, ok := strings.CutPrefix(s, "-----BEGIN SSH SIGNATURE-----")
	if !ok {
		return errors.New("not an SSH signature")
	}
	s, ok = strings.CutSuffix(s, "-----END SSH SIGNATURE-----")
	if !ok {
		return errors.New("not an SSH signature")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil || len(raw) < 10 || string(raw[:6]) != "SSHSIG" || binary.BigEndian.Uint32(raw[6:10]) != 1 {
		return errors.New("bad SSH signature")
	}
	rest := raw[10:]
	var pub, ns, reserved, hashAlg, sigBlob []byte
	for _, p := range []*[]byte{&pub, &ns, &reserved, &hashAlg, &sigBlob} {
		if *p, rest, err = sshString(rest); err != nil {
			return errors.New("bad SSH signature")
		}
	}
	if len(rest) != 0 || string(ns) != namespace {
		return errors.New("signature is not for " + namespace)
	}
	key, err := ed25519Key(pub)
	if err != nil {
		return err
	}
	pinned := false
	for _, k := range keys {
		f := strings.Fields(k)
		if len(f) < 2 {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(f[1]); err == nil {
			if pk, err := ed25519Key(b); err == nil && pk.Equal(key) {
				pinned = true
			}
		}
	}
	if !pinned {
		return errors.New("signed with an unknown key")
	}
	typ, sigRest, err := sshString(sigBlob)
	if err != nil || string(typ) != "ssh-ed25519" {
		return errors.New("not an ed25519 signature")
	}
	sig, _, err := sshString(sigRest)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("bad ed25519 signature")
	}
	var h []byte
	switch string(hashAlg) {
	case "sha512":
		x := sha512.Sum512(message)
		h = x[:]
	case "sha256":
		x := sha256.Sum256(message)
		h = x[:]
	default:
		return errors.New("unknown hash " + string(hashAlg))
	}
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	putSSHString(&signed, ns)
	putSSHString(&signed, reserved)
	putSSHString(&signed, hashAlg)
	putSSHString(&signed, h)
	if !ed25519.Verify(key, signed.Bytes(), sig) {
		return errors.New("signature does not match")
	}
	return nil
}

// verifyReleaseSums: SHA256SUMS izdaje s podpisom pripetega ključa.
func verifyReleaseSums(sums, sig []byte) error {
	return verifySSHSig(sums, sig, releaseKeys, releaseNamespace)
}
