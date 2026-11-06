package main

// Šifrirana povezava minigw ↔ router: Noise_NK_25519_AESGCM_SHA256 (»MGW2«), kot RouterCore/Noise.swift.
// AES-GCM je v standardni knjižnici Go, zato exe nima zunanjih odvisnosti. Router ima stalni ključ (javni del
// je v nizu »Copy Token«), odjemalec je anoniman in se overi z žetonom v šifriranem HTTP.
// Vsaka TCP povezava: »MGW2« + rokovanje (48 + 48 bajtov), nato okvirji.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
)

var (
	noiseMagic  = []byte("MGW2") // šifra AESGCM (router podpira tudi MGW1 = ChaChaPoly)
	deviceMagic = []byte("DEV1")
)

const (
	protocolName = "Noise_NK_25519_AESGCM_SHA256" // h0: z ničlami dopolnjeno na 32 bajtov (HASHLEN)
	handshakeLen = 48                             // e (32) + prazen šifriran payload (16)
	maxPlain     = 65535 - 16
)

var errDecrypt = errors.New("noise: decrypt failed")

// noiseCipher: smerni ključ s števcem (AESGCM v Noise: nonce = 4 ničle + 64-bitni števec big endian).
type noiseCipher struct {
	mu   sync.Mutex
	aead cipher.AEAD
	n    uint64
}

func newNoiseCipher(key []byte) *noiseCipher {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // ključ je vedno 32 bajtov
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &noiseCipher{aead: a}
}

func (c *noiseCipher) nonce() []byte {
	var n [12]byte
	binary.BigEndian.PutUint64(n[4:], c.n)
	return n[:]
}

func (c *noiseCipher) seal(plain, ad []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.aead.Seal(nil, c.nonce(), plain, ad)
	c.n++
	return out
}

func (c *noiseCipher) open(ct, ad []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(ct) < 16 {
		return nil, errDecrypt
	}
	p, err := c.aead.Open(nil, c.nonce(), ct, ad)
	c.n++
	if err != nil {
		return nil, errDecrypt
	}
	return p, nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func noiseHKDF(ck, ikm []byte) ([]byte, []byte) {
	temp := hmacSHA256(ck, ikm)
	o1 := hmacSHA256(temp, []byte{1})
	o2 := hmacSHA256(temp, append(append([]byte{}, o1...), 2))
	return o1, o2
}

type symmetric struct {
	ck, h []byte
	k     *noiseCipher
}

func (s *symmetric) mixHash(d []byte) {
	sum := sha256.Sum256(append(append([]byte{}, s.h...), d...))
	s.h = sum[:]
}

func (s *symmetric) mixKey(ikm []byte) {
	a, b := noiseHKDF(s.ck, ikm)
	s.ck = a
	s.k = newNoiseCipher(b)
}

func (s *symmetric) encryptAndHash(p []byte) []byte {
	c := s.k.seal(p, s.h)
	s.mixHash(c)
	return c
}

func (s *symmetric) decryptAndHash(c []byte) ([]byte, error) {
	p, err := s.k.open(c, s.h)
	if err != nil {
		return nil, err
	}
	s.mixHash(c)
	return p, nil
}

// noiseSession: promet po rokovanju; hash je h po rokovanju (vezava dokaza naprave na povezavo).
type noiseSession struct {
	send, receive *noiseCipher
	hash          []byte
}

type initiator struct {
	s  symmetric
	e  *ecdh.PrivateKey
	rs *ecdh.PublicKey
}

func newInitiator(routerKey []byte) (*initiator, error) {
	rs, err := ecdh.X25519().NewPublicKey(routerKey)
	if err != nil {
		return nil, err
	}
	e, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	h0 := make([]byte, 32)
	copy(h0, protocolName)
	i := &initiator{s: symmetric{ck: h0, h: append([]byte{}, h0...)}, e: e, rs: rs}
	i.s.mixHash(noiseMagic)
	i.s.mixHash(routerKey) // <- s: vnaprej znan ključ routerja
	return i, nil
}

// first: -> e, es
func (i *initiator) first() ([]byte, error) {
	pub := i.e.PublicKey().Bytes()
	i.s.mixHash(pub)
	dh, err := i.e.ECDH(i.rs)
	if err != nil {
		return nil, err
	}
	i.s.mixKey(dh)
	return append(pub, i.s.encryptAndHash(nil)...), nil
}

// finish: <- e, ee
func (i *initiator) finish(msg []byte) (*noiseSession, error) {
	if len(msg) != handshakeLen {
		return nil, errors.New("noise: bad handshake length")
	}
	re, err := ecdh.X25519().NewPublicKey(msg[:32])
	if err != nil {
		return nil, err
	}
	i.s.mixHash(msg[:32])
	dh, err := i.e.ECDH(re)
	if err != nil {
		return nil, err
	}
	i.s.mixKey(dh)
	if _, err := i.s.decryptAndHash(msg[32:]); err != nil {
		return nil, err
	}
	a, b := noiseHKDF(i.s.ck, nil)
	return &noiseSession{send: newNoiseCipher(a), receive: newNoiseCipher(b), hash: i.s.h}, nil
}

// frames: 2 bajta dolžina (big endian) + šifrirani del, največ maxPlain bajtov besedila na okvir.
func frames(plain []byte, c *noiseCipher) []byte {
	var out []byte
	for i := 0; ; {
		j := i + maxPlain
		if j > len(plain) {
			j = len(plain)
		}
		ct := c.seal(plain[i:j], nil)
		out = append(out, byte(len(ct)>>8), byte(len(ct)))
		out = append(out, ct...)
		i = j
		if i >= len(plain) {
			return out
		}
	}
}

// unframe: izlušči cele okvirje iz buf (ostanek vrne) in jih dešifrira.
func unframe(buf []byte, c *noiseCipher) (plain, rest []byte, err error) {
	for len(buf) >= 2 {
		n := int(buf[0])<<8 | int(buf[1])
		if len(buf) < 2+n {
			break
		}
		p, err := c.open(buf[2:2+n], nil)
		if err != nil {
			return nil, nil, err
		}
		plain = append(plain, p...)
		buf = buf[2+n:]
	}
	return plain, buf, nil
}

// deviceProof: »DEV1« + javni ključ naprave (Ed25519) + podpis nad hashem rokovanja + dolžina imena + ime.
func deviceProof(key ed25519.PrivateKey, name string, hash []byte) []byte {
	n := []byte(name)
	if len(n) > 255 {
		n = n[:255]
	}
	msg := append(append([]byte("minigw-device"), hash...), n...)
	sig := ed25519.Sign(key, msg)
	out := append(append([]byte{}, deviceMagic...), key.Public().(ed25519.PublicKey)...)
	out = append(out, sig...)
	out = append(out, byte(len(n)))
	return append(out, n...)
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func fromBase64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
