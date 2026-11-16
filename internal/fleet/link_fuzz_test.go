package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// ---------------------------------------------------------------------------
// FuzzParseKeys
//
// Invariants, for any bytes (raw, or a fuzzed DER body wrapped in a PEM block
// of a chosen type, with valid neighbour blocks around it):
//   - ParsePrivateKey and ParsePublicKeys never panic.
//   - Every key they accept is Ed25519 or ECDSA on P-256 or P-384: never RSA,
//     another curve or an exchange-only key.
//   - An accepted private key signs, and its own public half verifies that
//     signature (the key that is pinned is the key that proves).
//   - ParsePrivateKey uses the first PRIVATE KEY or EC PRIVATE KEY block and
//     only that one: it succeeds exactly when that block, decoded here on its
//     own, holds a signing key of an allowed kind, and then returns that
//     key. A refused first key is an error, never skipped for a later one.
//     (On an error it may also return the refused key: callers must check
//     the error, and every caller does.)
//   - ParsePublicKeys succeeds exactly when there is at least one PUBLIC KEY
//     block and every PUBLIC KEY block, decoded here on its own, holds a key
//     of an allowed kind; it then returns one key per block, in order, each
//     equal to that block's key. A refused block is an error, never skipped.
//   - Fingerprint is "SHA256:" + 43 base64 characters, the same on every call,
//     and equal to the SHA-256 of the key's PKIX form.
//   - PublicPEM of an accepted key parses back to exactly one key, equal to
//     it, with the same fingerprint.
// ---------------------------------------------------------------------------

var lfBlockTypes = []string{"PRIVATE KEY", "EC PRIVATE KEY", "PUBLIC KEY", "RSA PRIVATE KEY",
	"CERTIFICATE", "OPENSSH PRIVATE KEY", "PRIVATE KEY", "PUBLIC KEY"}

// lfSeedKeys are fixed keys of every kind the parsers meet (RSA is made once
// at random: only its bytes reach the parsers).
type lfSeedKeys struct {
	edPriv, edPub, p256Priv, p256Sec1, p256Pub, p384Priv, p384Pub       []byte
	p224Sec1, p224Pub, p521Priv, rsaPriv, rsaPub, x25519Priv, x25519Pub []byte
}

func lfScalar(n int, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, n)
	b[0] = 0 // below every curve's order
	b[1] = 1
	return b
}

func lfMakeSeedKeys(f *testing.F) lfSeedKeys {
	f.Helper()
	must := func(b []byte, err error) []byte {
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	var k lfSeedKeys
	ed := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	k.edPriv = must(x509.MarshalPKCS8PrivateKey(ed))
	k.edPub = must(x509.MarshalPKIXPublicKey(ed.Public()))
	ec := func(c elliptic.Curve, n int) *ecdsa.PrivateKey {
		p, err := ecdsa.ParseRawPrivateKey(c, lfScalar(n, 0x5a))
		if err != nil {
			f.Fatal(err)
		}
		return p
	}
	p256, p384, p521 := ec(elliptic.P256(), 32), ec(elliptic.P384(), 48), ec(elliptic.P521(), 66)
	k.p256Priv = must(x509.MarshalPKCS8PrivateKey(p256))
	k.p256Sec1 = must(x509.MarshalECPrivateKey(p256))
	k.p256Pub = must(x509.MarshalPKIXPublicKey(&p256.PublicKey))
	k.p384Priv = must(x509.MarshalPKCS8PrivateKey(p384))
	k.p384Pub = must(x509.MarshalPKIXPublicKey(&p384.PublicKey))
	k.p521Priv = must(x509.MarshalPKCS8PrivateKey(p521))
	// P-224 (the default curve of a common key generator) has no raw
	// parser: a random key; only its bytes matter.
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	k.p224Sec1 = must(x509.MarshalECPrivateKey(p224))
	k.p224Pub = must(x509.MarshalPKIXPublicKey(&p224.PublicKey))
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatal(err)
	}
	k.rsaPriv = must(x509.MarshalPKCS8PrivateKey(r))
	k.rsaPub = must(x509.MarshalPKIXPublicKey(&r.PublicKey))
	x, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		f.Fatal(err)
	}
	k.x25519Priv = must(x509.MarshalPKCS8PrivateKey(x))
	k.x25519Pub = must(x509.MarshalPKIXPublicKey(x.PublicKey()))
	return k
}

// lfKeyInput turns (mode, body) into the bytes the parsers read. mode's top
// bit: body as is. Otherwise body is a DER wrapped in a block of type
// lfBlockTypes[mode&7]; 0x08 puts a valid P-256 PUBLIC KEY before it, 0x10 a
// valid Ed25519 PUBLIC KEY after it, 0x20 a valid Ed25519 PRIVATE KEY after
// it, 0x40 gives the block PEM headers.
func lfKeyInput(k lfSeedKeys, mode uint8, body []byte) []byte {
	if mode&0x80 != 0 {
		return body
	}
	var out []byte
	if mode&0x08 != 0 {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.p256Pub})...)
	}
	blk := &pem.Block{Type: lfBlockTypes[mode&7], Bytes: body}
	if mode&0x40 != 0 {
		blk.Headers = map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}
	}
	out = append(out, pem.EncodeToMemory(blk)...)
	if mode&0x10 != 0 {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.edPub})...)
	}
	if mode&0x20 != 0 {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k.edPriv})...)
	}
	return out
}

// lfAllowed is the key policy, written out independently of checkPublic.
func lfAllowed(k crypto.PublicKey) bool {
	switch p := k.(type) {
	case ed25519.PublicKey:
		return len(p) == ed25519.PublicKeySize
	case *ecdsa.PublicKey:
		n := p.Curve.Params().Name
		return n == "P-256" || n == "P-384"
	}
	return false
}

func lfCheckPublic(t *testing.T, k crypto.PublicKey) {
	t.Helper()
	if !lfAllowed(k) {
		t.Fatalf("a %T key was accepted (%v)", k, k)
	}
	fp := Fingerprint(k)
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) != 50 {
		t.Fatalf("fingerprint %q", fp)
	}
	if again := Fingerprint(k); again != fp {
		t.Fatalf("fingerprint not stable: %q then %q", fp, again)
	}
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatalf("an accepted key does not marshal: %v", err)
	}
	sum := sha256.Sum256(der)
	if Fingerprint(k) != "SHA256:"+strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=") {
		t.Fatalf("fingerprint %q is not the SHA-256 of the PKIX form", fp)
	}
	p, err := PublicPEM(k)
	if err != nil {
		t.Fatalf("PublicPEM of an accepted key: %v", err)
	}
	back, err := ParsePublicKeys(p)
	if err != nil || len(back) != 1 {
		t.Fatalf("PublicPEM does not parse back: %v (%d keys)\n%s", err, len(back), p)
	}
	eq, ok := k.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !eq.Equal(back[0]) {
		t.Fatalf("PublicPEM round trip changed the key: %T", back[0])
	}
	if Fingerprint(back[0]) != fp {
		t.Fatalf("round trip changed the fingerprint: %q to %q", fp, Fingerprint(back[0]))
	}
}

// lfPrivateOracle decodes the first PRIVATE KEY or EC PRIVATE KEY block of
// in by itself: found says there is one, pub is its public half when it
// parses as a signing key, ok that the key is of an allowed kind.
func lfPrivateOracle(in []byte) (pub crypto.PublicKey, found, ok bool) {
	for rest := in; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return nil, false, false
		}
		var k any
		var err error
		switch b.Type {
		case "PRIVATE KEY":
			k, err = x509.ParsePKCS8PrivateKey(b.Bytes)
		case "EC PRIVATE KEY":
			k, err = x509.ParseECPrivateKey(b.Bytes)
		default:
			continue
		}
		s, signer := k.(crypto.Signer)
		if err != nil || !signer {
			return nil, true, false
		}
		return s.Public(), true, lfAllowed(s.Public())
	}
}

// lfPublicOracle decodes every PUBLIC KEY block of in by itself: the keys
// in order, and whether every block holds a key of an allowed kind.
func lfPublicOracle(in []byte) (keys []crypto.PublicKey, allOK bool) {
	allOK = true
	for rest := in; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return keys, allOK
		}
		if b.Type != "PUBLIC KEY" {
			continue
		}
		k, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil || !lfAllowed(k) {
			allOK = false
		}
		keys = append(keys, k)
	}
}

func lfEqual(a, b crypto.PublicKey) bool {
	eq, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && eq.Equal(b)
}

func FuzzParseKeys(f *testing.F) {
	k := lfMakeSeedKeys(f)
	// Mode: block type index (0 PRIVATE KEY, 1 EC PRIVATE KEY, 2 PUBLIC KEY).
	for _, s := range []struct {
		mode uint8
		body []byte
	}{
		{0, k.edPriv}, {0, k.p256Priv}, {1, k.p256Sec1}, {0, k.p384Priv},
		{2, k.edPub}, {2, k.p256Pub}, {2, k.p384Pub},
		{2 | 0x08 | 0x10, k.p384Pub}, // three public keys: a key being replaced
		// Refused kinds (the audit's key policy): RSA, P-224, P-521, X25519.
		{0, k.rsaPriv}, {2, k.rsaPub}, {1, k.p224Sec1}, {2, k.p224Pub},
		{0, k.p521Priv}, {0, k.x25519Priv}, {2, k.x25519Pub},
		// A refused key before a good one: refused, not skipped.
		{2 | 0x10, k.rsaPub}, {0 | 0x20, k.rsaPriv},
		{2 | 0x08, k.rsaPub}, {2 | 0x08 | 0x10, k.p224Pub}, {1 | 0x20, k.p224Sec1},
		{0 | 0x20, k.x25519Priv}, {2 | 0x10, k.p256Pub[:30]}, {0 | 0x20, k.edPriv[:10]},
		{4 | 0x20, k.edPub}, // a good private key after a block of another type
		// Wrong block types, headers, truncation, empty.
		{3, k.p256Sec1}, {4, k.edPub}, {5, k.edPriv}, {1 | 0x40, k.p256Sec1},
		{0, k.edPriv[:len(k.edPriv)-1]}, {2, k.p256Pub[:20]}, {2, nil},
		{0x80, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG")},
		{0x80, []byte("-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----\n")},
		{0x80, []byte("-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEI\n-----END PRIVATE KEY-----\n")},
		{0x80, nil},
	} {
		f.Add(s.mode, s.body)
	}
	f.Fuzz(func(t *testing.T, mode uint8, body []byte) {
		in := lfKeyInput(k, mode, body)
		wantPub, found, wantOK := lfPrivateOracle(in)
		priv, err := ParsePrivateKey(in)
		switch {
		case err == nil && !wantOK:
			t.Fatalf("ParsePrivateKey accepted a %T key although the first private key block (found %v, %T) is not an allowed key", priv, found, wantPub)
		case err != nil && wantOK:
			t.Fatalf("ParsePrivateKey refused an allowed %T key in the first private key block: %v", wantPub, err)
		}
		if err == nil {
			if priv == nil {
				t.Fatal("ParsePrivateKey: nil key and no error")
			}
			pub := priv.Public()
			if !lfEqual(wantPub, pub) {
				t.Fatalf("ParsePrivateKey returned a key that is not the first private key block's (%T)", pub)
			}
			lfCheckPublic(t, pub)
			msg := []byte("the hub's challenge")
			switch p := pub.(type) {
			case ed25519.PublicKey:
				sig, err := priv.Sign(rand.Reader, msg, crypto.Hash(0))
				if err != nil || !ed25519.Verify(p, msg, sig) {
					t.Fatalf("an accepted Ed25519 key does not sign for its public half: %v", err)
				}
			case *ecdsa.PublicKey:
				d := sha256.Sum256(msg)
				sig, err := priv.Sign(rand.Reader, d[:], crypto.SHA256)
				if err != nil || !ecdsa.VerifyASN1(p, d[:], sig) {
					t.Fatalf("an accepted ECDSA key does not sign for its public half: %v", err)
				}
			}
		}
		want, allOK := lfPublicOracle(in)
		wantOK = allOK && len(want) > 0
		pubs, err := ParsePublicKeys(in)
		if err != nil {
			if pubs != nil {
				t.Fatalf("ParsePublicKeys: keys and an error: %v", err)
			}
			if wantOK {
				t.Fatalf("ParsePublicKeys refused %d PUBLIC KEY blocks that all hold allowed keys: %v", len(want), err)
			}
			return
		}
		if !wantOK {
			t.Fatalf("ParsePublicKeys accepted %d keys although one of its %d PUBLIC KEY blocks is refused (or there is none)", len(pubs), len(want))
		}
		if len(pubs) != len(want) {
			t.Fatalf("ParsePublicKeys: %d keys from %d PUBLIC KEY blocks", len(pubs), len(want))
		}
		for i, p := range pubs {
			if !lfEqual(want[i], p) {
				t.Fatalf("ParsePublicKeys: key %d is not block %d's key (%T)", i, i, p)
			}
			lfCheckPublic(t, p)
		}
	})
}

// ---------------------------------------------------------------------------
// FuzzConnectQuery
//
// The hub's connect handler with any method, query, WebSocket headers, extra
// header and Host, on a ResponseWriter that records an upgrade attempt
// (Hijack) and refuses it. Invariants:
//   - A request whose name, store, version or protocol is not of the forms
//     members send (checked here by a hand-written oracle, not the hub's
//     patterns) is refused with 400 or 409 and the hub's own text, before any
//     upgrade: Hijack is never called.
//   - Hijack is only ever called for a request the oracle accepts; with
//     maxPending connections pending it is not called either (503).
//   - The status is one of 101 (upgrade attempted), 400, 403, 405, 409, 426,
//     503.
//   - Nothing reaches the registry or the link table: Member and Up are
//     never called.
//   - The pending counter is the same after the request as before it.
//   - The refusal text is valid UTF-8 and short, with no control character
//     (other than its final newline), no C1 control and no bidi override:
//     whatever the request carried is never echoed raw.
// ---------------------------------------------------------------------------

// lfConnWriter is a ResponseWriter that records what the hub does and
// refuses to hand over the connection.
type lfConnWriter struct {
	h        http.Header
	code     int
	body     bytes.Buffer
	hijacked bool
}

func (w *lfConnWriter) Header() http.Header { return w.h }
func (w *lfConnWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(b)
}
func (w *lfConnWriter) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
}
func (w *lfConnWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, errors.New("no connection in this test")
}

// lfOracle says whether a connect query carries values of the forms members
// send, written out by hand (independent of the hub's regular expressions).
func lfOracle(q url.Values) bool {
	name, store, version := q.Get("name"), q.Get("store"), q.Get("version")
	if n := len(name); n < 1 || n > 40 || name[0] == '-' || name[n-1] == '-' {
		return false
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	if len(store) != 12 {
		return false
	}
	for _, c := range []byte(store) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	if len(version) > 64 {
		return false
	}
	for _, c := range []byte(version) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.IndexByte(".+_() -", c) >= 0) {
			return false
		}
	}
	p, err := strconv.Atoi(q.Get("protocol"))
	if err != nil {
		return false
	}
	for _, s := range Protocols() {
		if p == s {
			return true
		}
	}
	return false
}

func lfSafeText(t *testing.T, b []byte) {
	t.Helper()
	// The hub's own refusals are fixed texts; the WebSocket library's quote
	// the offending header (escaped), so only their characters are checked.
	if s := string(b); len(b) > 256 && (strings.HasPrefix(s, "invalid ") ||
		strings.HasPrefix(s, "this hub speaks") || strings.HasPrefix(s, "too many")) {
		t.Fatalf("a refusal of %d bytes: %.120q", len(b), b)
	}
	if !utf8.Valid(b) {
		t.Fatalf("a refusal that is not UTF-8: %q", b)
	}
	s := strings.TrimSuffix(string(b), "\n")
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f ||
			r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c {
			t.Fatalf("the refusal echoes the control character %U: %q", r, b)
		}
	}
}

const (
	lfConnection = 1 << iota // Connection: Upgrade
	lfUpgrade                // Upgrade: websocket
	lfWSVersion              // Sec-WebSocket-Version: 13
	lfWSKey                  // Sec-WebSocket-Key (wsKey, or a valid one when empty)
	lfHTTP10                 // HTTP/1.0
	lfFull                   // maxPending connections already pending
	lfHost                   // Host: hValue
	lfRawQuery               // the query is raw, not built from the fields
)

func FuzzConnectQuery(f *testing.F) {
	ws := uint16(lfConnection | lfUpgrade | lfWSVersion | lfWSKey)
	good := []string{"GET", "eu", "0123456789ab", "0.7.0", "1"}
	add := func(m, name, store, version, proto, raw string, flags uint16, key, hn, hv string) {
		f.Add(m, name, store, version, proto, raw, flags, key, hn, hv)
	}
	add(good[0], good[1], good[2], good[3], good[4], "", ws, "", "", "")
	add(good[0], good[1], good[2], good[3], good[4], "", 0, "", "", "")
	add(good[0], good[1], good[2], good[3], good[4], "", ws|lfFull, "", "", "")
	// Known-bad values from the audit: forged lines, bidi, sizes, case.
	add("GET", "eu", "store aaaa -> bbbb\nforged", "test", "1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "0.7.0\nforged line", "1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "0.7.0\u202egnp.exe", "1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", strings.Repeat("9", 65), "1", "", ws, "", "", "")
	add("GET", strings.Repeat("a", 5000), "0123456789ab", "test", "1", "", ws, "", "", "")
	add("GET", "EU", "0123456789ab", "test", "1", "", ws, "", "", "")
	add("GET", "eu-", "0123456789AB", "test", "1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "0", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "+1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "99999999999999999999", "", ws, "", "", "")
	// Duplicates and separators: the first value counts, on both sides.
	add("GET", "eu", "0123456789ab", "test", "1", "name=../x&store=zz", ws, "", "", "")
	add("GET", "", "", "", "", "store=0123456789ab&name=eu&name=evil%0a&version=t&protocol=1", ws|lfRawQuery, "", "", "")
	add("GET", "", "", "", "", "name=eu;store=0123456789ab&version=t&protocol=1", ws|lfRawQuery, "", "", "")
	add("GET", "", "", "", "", "%zz&name=eu&store=0123456789ab&version=t&protocol=1", ws|lfRawQuery, "", "", "")
	// WebSocket refusals that quote the request: method, key, Origin, proto.
	add("POST\r\nX: y", "eu", "0123456789ab", "test", "1", "", ws, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "1", "", ws, "bad\x1b[31mkey\u202e", "", "")
	add("GET", "eu", "0123456789ab", "test", "1", "", ws, "", "Origin", "http://evil\x1b[2J.example\u202e")
	add("GET", "eu", "0123456789ab", "test", "1", "", ws|lfHost, "", "Origin", "http://a.example\n")
	add("GET", "eu", "0123456789ab", "test", "1", "", ws|lfHTTP10, "", "", "")
	add("GET", "eu", "0123456789ab", "test", "1", "", ws, "", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	add("GET", "eu", "0123456789ab", "test", "1", "", lfConnection|lfUpgrade|lfWSKey, "", "Sec-WebSocket-Version", "13\x00")

	priv, _, err := GenerateKey()
	if err != nil {
		f.Fatal(err)
	}
	key, _ := ParsePrivateKey(priv)
	var asked, ups atomic.Int32
	h := &Hub{Key: key, Log: quiet,
		Member: func(string) (string, error) { asked.Add(1); return "", errors.New("none") },
		Up:     func(*Link) { ups.Add(1) }}

	f.Fuzz(func(t *testing.T, method, name, store, version, proto, raw string, flags uint16, wsKey, hName, hValue string) {
		q := raw
		if flags&lfRawQuery == 0 {
			q = url.Values{"name": {name}, "store": {store}, "version": {version}, "protocol": {proto}}.Encode()
			if raw != "" {
				q += "&" + raw
			}
		}
		r := &http.Request{Method: method, URL: &url.URL{Path: ConnectPath, RawQuery: q},
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
			Host: "hub.example", RemoteAddr: "192.0.2.1:4242"}
		if flags&lfHTTP10 != 0 {
			r.Proto, r.ProtoMinor = "HTTP/1.0", 0
		}
		if flags&lfConnection != 0 {
			r.Header.Set("Connection", "Upgrade")
		}
		if flags&lfUpgrade != 0 {
			r.Header.Set("Upgrade", "websocket")
		}
		if flags&lfWSVersion != 0 {
			r.Header.Set("Sec-WebSocket-Version", "13")
		}
		if flags&lfWSKey != 0 {
			if wsKey == "" {
				wsKey = "dGhlIHNhbXBsZSBub25jZQ=="
			}
			r.Header.Set("Sec-WebSocket-Key", wsKey)
		}
		if hName != "" {
			r.Header.Add(hName, hValue)
		}
		if flags&lfHost != 0 {
			r.Host = hValue
		}
		var preset int32
		if flags&lfFull != 0 {
			preset = maxPending
		}
		h.pending.Store(preset)
		askedBefore, upsBefore := asked.Load(), ups.Load()
		w := &lfConnWriter{h: http.Header{}}
		h.ServeHTTP(w, r)

		valid := lfOracle(r.URL.Query())
		switch w.code {
		case http.StatusSwitchingProtocols, http.StatusBadRequest, http.StatusForbidden,
			http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUpgradeRequired, http.StatusServiceUnavailable:
		default:
			t.Fatalf("status %d (%q)", w.code, w.body.String())
		}
		if w.hijacked != (w.code == http.StatusSwitchingProtocols) {
			t.Fatalf("status %d, upgrade attempted: %v", w.code, w.hijacked)
		}
		if w.hijacked && !valid {
			t.Fatalf("the hub upgraded a connect request with query %q", q)
		}
		if w.hijacked && preset >= maxPending {
			t.Fatal("the hub upgraded a connection with maxPending already pending")
		}
		if !valid {
			body := w.body.String()
			if (w.code != http.StatusBadRequest || !strings.HasPrefix(body, "invalid ")) &&
				(w.code != http.StatusConflict || !strings.HasPrefix(body, "this hub speaks protocol")) {
				t.Fatalf("query %q refused with %d %q, want the hub's own 400 or 409", q, w.code, body)
			}
		}
		wsAll := uint16(lfConnection | lfUpgrade | lfWSVersion | lfWSKey)
		if k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(wsKey)); valid && err == nil && len(k) == 16 &&
			flags&(wsAll|lfHTTP10|lfFull|lfHost) == wsAll && method == "GET" && hName == "" && !w.hijacked {
			t.Fatalf("a conforming connect request (query %q) was refused: %d %q", q, w.code, w.body.String())
		}
		if asked.Load() != askedBefore || ups.Load() != upsBefore {
			t.Fatal("a connect request that never upgraded reached Member or Up")
		}
		if p := h.pending.Load(); p != preset {
			t.Fatalf("pending %d after the request, %d before", p, preset)
		}
		lfSafeText(t, w.body.Bytes())
	})
}

// ---------------------------------------------------------------------------
// FuzzLinkPeerFrames
//
// A raw member dials a real hub (httptest, loopback, shared across inputs),
// completes the pinned handshake, then misbehaves as the input says:
//
//	data[0] & 3        0 structured yamux frames, 1 answer the hub's HTTP
//	                   request with data[2:] (data[1]: what to do after),
//	                   2 raw bytes on the session, 3 garbage before TLS
//	(data[0] >> 2) & 3 0 the registered Ed25519 key under its own name,
//	                   1 the registered P-256 key under its own name,
//	                   2 the registered Ed25519 key claiming another name,
//	                   3 a key nobody registered
//
// Determinism: in modes 0 and 2 the hub's request starts first and the
// member waits until it has seen the hub open stream 1 (its SYN), so frames
// naming stream 1 always meet the hub's open stream and never race its
// opening. Every wait is for an event (a frame, a closed channel, a handler
// that returned); the durations below are failure bounds, never sleeps. The
// only wait by design is mode 1's held answers, which end at the hub's
// 100 ms client deadline.
//
// Invariants:
//   - The hub never panics.
//   - Garbage before TLS never reaches Member and never makes a link; every
//     TLS handshake asks Member exactly once.
//   - A key under another member's name, or an unknown key, never gets the
//     hub's confirmation byte and never makes a link; a registered key under
//     its own name (either kind) always does, under that name.
//   - While a link is up nothing is pending: pending counts only connections
//     whose key the hub has not judged yet (else 64 connected members would
//     lock every new one out with 503).
//   - A hub request through the link ends within its deadline plus a margin
//     whatever the member answers (mode 1), and within a margin of the link
//     ending (modes 0 and 2).
//   - After only frames the hub reads through, a member that opens a stream
//     to the hub (a SYN on a data or window frame), sends a frame of another
//     yamux version or a GoAway with an error code loses its link.
//   - After only well-formed frames that cannot break the session, the hub
//     keeps the link and keeps reading: a ping sent after them is answered,
//     and the link is still up then.
//   - Once the member hangs up, the hub's handler returns, pending is zero,
//     the link is Done and no other link came up.
// ---------------------------------------------------------------------------

type lfRig struct {
	srv      *httptest.Server
	hub      *Hub
	hubFP    string
	member   tls.Certificate // Ed25519, registered as "eu"
	memberEC tls.Certificate // P-256, registered as "eu" too
	stranger tls.Certificate
	links    chan *Link
	returned chan struct{}
	asked    atomic.Int32
	seq      uint32
}

func lfNewRig(f *testing.F) *lfRig {
	f.Helper()
	keyOf := func(fill byte) crypto.Signer {
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize))
	}
	hubKey, memberKey, strangerKey := keyOf(1), keyOf(2), keyOf(3)
	ecKey, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), lfScalar(32, 0x33))
	if err != nil {
		f.Fatal(err)
	}
	rig := &lfRig{links: make(chan *Link, 4), returned: make(chan struct{}, 4), hubFP: Fingerprint(hubKey.Public())}
	if rig.member, err = certificate("eu", memberKey); err != nil {
		f.Fatal(err)
	}
	if rig.memberEC, err = certificate("eu", ecKey); err != nil {
		f.Fatal(err)
	}
	if rig.stranger, err = certificate("eu", strangerKey); err != nil {
		f.Fatal(err)
	}
	registered := map[string]bool{Fingerprint(memberKey.Public()): true, Fingerprint(ecKey.Public()): true}
	rig.hub = &Hub{Key: hubKey, Log: quiet,
		Member: func(fp string) (string, error) {
			rig.asked.Add(1)
			if registered[fp] {
				return "eu", nil
			}
			return "", errors.New("no member holds this key")
		},
		Up: func(l *Link) { rig.links <- l }}
	mux := http.NewServeMux()
	mux.Handle("GET "+ConnectPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { rig.returned <- struct{}{} }()
		rig.hub.ServeHTTP(w, r)
	}))
	rig.srv = httptest.NewServer(mux)
	f.Cleanup(rig.srv.Close)
	return rig
}

// lfBarrier is the top byte of the member's barrier ping ids; lfFrames never
// sends a ping id with it.
const lfBarrier = 0xfb

// lfFrames turns bytes into yamux frames (12-byte header: version, type,
// flags, stream id, length; data frames carry their payload). It reports
// whether the hub must end the link and whether every frame up to that
// point is one the hub reads through with an outcome that does not depend
// on timing: no data frame claims more bytes than it carries, and no second
// FIN reaches stream 1 (whether that breaks the session depends on whether
// the hub's own side has closed the stream meanwhile).
func lfFrames(b []byte) (out []byte, mustClose, reliable bool) {
	next := func() byte {
		if len(b) == 0 {
			return 0
		}
		c := b[0]
		b = b[1:]
		return c
	}
	take := func(n int) []byte {
		p := make([]byte, n)
		copy(p, b)
		b = b[min(n, len(b)):]
		return p
	}
	reliable = true
	fin1, rst1 := false, false // stream 1, the hub's request, has seen a FIN / an RST
	for n := 0; len(b) > 0 && n < 32; n++ {
		c := next()
		typ := c & 3
		var ver byte
		if c&0x80 != 0 {
			ver = 1 + (c>>4)&3
		}
		lie := c&0x40 != 0
		flags := uint16(next() & 0x0f) // SYN 1, ACK 2, FIN 4, RST 8
		id := uint32(next())
		if id == 0xff {
			id = 0xffffffff
		}
		var length uint32
		var payload []byte
		switch typ {
		case 0: // data
			payload = take(int(next() % 64))
			length = uint32(len(payload))
			if lie {
				length = 0xffffff00 | uint32(next())
			}
		case 1, 2: // window update, ping
			length = binary.BigEndian.Uint32(take(4))
			if typ == 2 && length>>24 == lfBarrier {
				length ^= 1 << 24
			}
		case 3: // go away
			length = uint32(next() % 4)
		}
		hdr := make([]byte, 12)
		hdr[0], hdr[1] = ver, typ
		binary.BigEndian.PutUint16(hdr[2:], flags)
		binary.BigEndian.PutUint32(hdr[4:], id)
		binary.BigEndian.PutUint32(hdr[8:], length)
		out = append(append(out, hdr...), payload...)
		if !reliable || mustClose {
			continue
		}
		switch {
		case ver != 0:
			mustClose = true
		case typ <= 1 && flags&1 != 0:
			mustClose = true
		case typ == 3 && length != 0:
			mustClose = true
		}
		if mustClose {
			continue
		}
		if typ == 0 && lie {
			reliable = false
		}
		if typ <= 1 && id == 1 && !rst1 {
			// yamux applies FIN before RST within one frame.
			if flags&4 != 0 {
				if fin1 {
					reliable = false
				}
				fin1 = true
			}
			if flags&8 != 0 {
				rst1 = true
			}
		}
	}
	return out, mustClose, reliable
}

// lfDrain reads the hub's yamux frames: it reports the hub opening stream 1
// and the pong to the barrier ping id.
func lfDrain(r io.Reader, id uint32, syn, pong chan<- struct{}) {
	hdr := make([]byte, 12)
	notify := func(c chan<- struct{}) {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		if hdr[0] != 0 {
			io.Copy(io.Discard, r)
			return
		}
		flags := binary.BigEndian.Uint16(hdr[2:])
		sid, length := binary.BigEndian.Uint32(hdr[4:]), binary.BigEndian.Uint32(hdr[8:])
		if hdr[1] <= 1 && flags&1 != 0 && sid == 1 {
			notify(syn)
		}
		switch hdr[1] {
		case 0:
			if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
				return
			}
		case 2:
			if flags&2 != 0 && length == id {
				notify(pong)
			}
		}
	}
}

// ask starts a hub request through the link; the channel says when it ended.
func lfAsk(link *Link, deadline time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := link.HTTP(deadline).Get("http://member/api")
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
		}
	}()
	return done
}

func (rig *lfRig) run(t *testing.T, data []byte) {
	if len(data) == 0 {
		data = []byte{0}
	}
	mode, identity := data[0]&3, (data[0]>>2)&3
	rest := data[1:]
	for len(rig.links) > 0 { // nothing from an earlier input
		<-rig.links
	}
	for len(rig.returned) > 0 {
		<-rig.returned
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := "eu"
	if identity == 2 {
		name = "us"
	}
	q := url.Values{"name": {name}, "store": {testStore}, "version": {"fuzz"}, "protocol": {strconv.Itoa(Protocol)}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(rig.srv.URL, "http")+ConnectPath+"?"+q.Encode(),
		&websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	askedBefore := rig.asked.Load()

	// finish hangs up and checks that the hub lets go of everything and
	// asked Member as often as there were handshakes.
	finish := func(link *Link, handshakes int32) {
		conn.Close()
		select {
		case <-rig.returned:
		case <-time.After(3 * time.Second):
			t.Fatal("the hub's handler did not return 3 s after the member hung up")
		}
		if p := rig.hub.pending.Load(); p != 0 {
			t.Fatalf("pending %d after the connection ended", p)
		}
		if link != nil {
			select {
			case <-link.Done():
			default:
				t.Fatal("the handler returned with the link still open")
			}
		}
		select {
		case l := <-rig.links:
			if link == nil {
				t.Fatalf("a link came up for %q", l.Name)
			}
			t.Fatalf("a second link came up for %q", l.Name)
		default:
		}
		if got := rig.asked.Load() - askedBefore; got != handshakes {
			t.Fatalf("Member was asked %d times for %d handshakes (mode %d, identity %d)", got, handshakes, mode, identity)
		}
	}

	if mode == 3 {
		if len(rest) > 0 {
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			conn.Write(rest)
		}
		finish(nil, 0)
		return
	}

	cert := rig.member
	switch identity {
	case 1:
		cert = rig.memberEC
	case 3:
		cert = rig.stranger
	}
	tc := tls.Client(conn, pinned(cert, false, func(fp string) error {
		if fp != rig.hubFP {
			return errors.New("not the hub's key")
		}
		return nil
	}))
	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	herr := tc.HandshakeContext(hctx)
	hcancel()
	var one [1]byte
	if herr == nil {
		tc.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, herr = io.ReadFull(tc, one[:])
		tc.SetReadDeadline(time.Time{})
	}
	if identity >= 2 {
		if herr == nil {
			t.Fatalf("identity %d (name %q) got the hub's confirmation %#x", identity, name, one[0])
		}
		finish(nil, 1)
		return
	}
	if herr != nil || one[0] != accepted {
		t.Fatalf("the registered member (identity %d) was not confirmed: %v %#x", identity, herr, one[0])
	}
	var link *Link
	select {
	case link = <-rig.links:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed, but no link came up")
	}
	if link.Name != "eu" {
		t.Fatalf("link named %q", link.Name)
	}
	if p := rig.hub.pending.Load(); p != 0 {
		t.Fatalf("pending %d while the link is up: a judged connection still counts as pending", p)
	}
	ended := func(asked <-chan struct{}, within time.Duration, what string) {
		select {
		case <-asked:
		case <-time.After(within):
			t.Fatalf("a hub request had not ended %s after %s", within, what)
		}
	}

	switch mode {
	case 0, 2:
		rig.seq++
		ping := lfBarrier<<24 | rig.seq&0xffffff
		syn, pong := make(chan struct{}, 1), make(chan struct{}, 1)
		go lfDrain(tc, ping, syn, pong)
		// A long client deadline: the request ends when the link does.
		asked := lfAsk(link, 10*time.Second)
		select {
		case <-syn:
		case <-time.After(2 * time.Second):
			t.Fatal("the hub's request did not open stream 1 within 2 s")
		}
		var frames []byte
		mustClose, reliable := false, false
		if mode == 0 {
			frames, mustClose, reliable = lfFrames(rest)
		} else {
			frames = rest
		}
		tc.SetWriteDeadline(time.Now().Add(2 * time.Second))
		tc.Write(frames)
		switch {
		case mustClose:
			select {
			case <-link.Done():
			case <-time.After(2 * time.Second):
				t.Fatalf("the hub kept the link 2 s after frames %x", frames)
			}
		case reliable:
			hdr := make([]byte, 12)
			hdr[1] = 2
			binary.BigEndian.PutUint16(hdr[2:], 1)
			binary.BigEndian.PutUint32(hdr[8:], ping)
			tc.Write(hdr)
			select {
			case <-pong:
			case <-link.Done():
				t.Fatalf("the hub ended the link after well-formed frames %x", frames)
			case <-time.After(2 * time.Second):
				t.Fatalf("the hub stopped reading the link after frames %x", frames)
			}
			select {
			case <-link.Done():
				t.Fatalf("the hub ended the link after well-formed frames %x", frames)
			default:
			}
		}
		conn.Close()
		ended(asked, 2*time.Second, "the member hung up")
	case 1:
		var action byte
		var answer []byte
		if len(rest) > 0 {
			action, answer = rest[0], rest[1:]
		}
		sess, err := yamux.Server(tc, muxConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		go func() {
			for {
				s, err := sess.Accept()
				if err != nil {
					return
				}
				go func() {
					s.SetDeadline(time.Now().Add(2 * time.Second))
					if action&4 == 0 {
						http.ReadRequest(bufio.NewReader(s))
					}
					s.Write(answer)
					switch action & 3 {
					case 0:
						s.Close()
					case 1: // keeps the answer open: the audit's held caller
					case 2:
						sess.Close()
					case 3:
						s.Write(answer)
						s.Close()
					}
				}()
			}
		}()
		ended(lfAsk(link, 100*time.Millisecond), 2*time.Second, "its 100 ms deadline")
		sess.Close()
	}
	finish(link, 1)
}

func FuzzLinkPeerFrames(f *testing.F) {
	rig := lfNewRig(f)
	frame := func(ver, typ byte, flags uint16, id, length uint32, payload ...byte) []byte {
		h := make([]byte, 12)
		h[0], h[1] = ver, typ
		binary.BigEndian.PutUint16(h[2:], flags)
		binary.BigEndian.PutUint32(h[4:], id)
		binary.BigEndian.PutUint32(h[8:], length)
		return append(h, payload...)
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	ok := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	for _, s := range [][]byte{
		// Structured frames (mode 0): byte, flags, id, then per type.
		{0, 0x00, 0x01, 2, 5, 'h', 'e', 'l', 'l', 'o'},                     // the member opens a stream (SYN data)
		{0, 0x01, 0x01, 4, 0, 0, 0, 0},                                     // SYN window update
		{0, 0x00, 0x01, 1, 0},                                              // SYN on the hub's own stream 1
		{0, 0x01, 0x00, 1, 0xff, 0xff, 0xff, 0xff},                         // huge window on the hub's own stream
		{0, 0x01, 0x00, 3, 0xff, 0xff, 0xff, 0xff},                         // huge window on a stream nobody opened
		{0, 0x00, 0x00, 1, 63},                                             // data on the hub's stream
		{0, 0x00, 0x02, 1, 38, 'H', 'T', 'T', 'P', '/'},                    // ACK and the start of an answer
		{0, 0x40, 0x00, 1, 0, 0x10},                                        // data claiming 4 GiB on the hub's stream
		{0, 0x40, 0x00, 9, 0, 0x10},                                        // data claiming 4 GiB on no stream
		{0, 0x02, 0x01, 0, 1, 2, 3, 4},                                     // a ping
		{0, 0x02, 0x01, 0, lfBarrier, 0, 0, 1},                             // a ping with a barrier-like id
		{0, 0x03, 0x00, 0, 0},                                              // go away, normal
		{0, 0x03, 0x00, 0, 1},                                              // go away, protocol error
		{0, 0x03, 0x00, 0, 3},                                              // go away, unknown code
		{0, 0x90, 0x00, 1, 0},                                              // another version
		{0, 0x00, 0x09, 1, 0},                                              // SYN+RST on the hub's own stream
		{0, 0x00, 0x04, 1, 0, 0x00, 0x04, 1, 0},                            // FIN twice (timing decides: not judged)
		{0, 0x00, 0x04, 1, 0, 0x01, 0x08, 1, 0, 0, 0, 0, 0x00, 0x04, 1, 0}, // FIN, RST, FIN: link stays
		{0, 0x01, 0x08, 1, 0, 0, 0, 0, 0x00, 0x01, 2, 0},                   // RST, then a stream
		{0, 0x00, 0x08, 1, 0, 0x00, 0x00, 1, 4, 1, 2, 3, 4},                // RST, then data for the gone stream
		// Answers (mode 1): action, then the bytes.
		append([]byte{1, 0}, ok...),
		append([]byte{1, 1}, "HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n{"...),
		append([]byte{1, 1}, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"...),
		append([]byte{1, 3}, ok...), // two answers to one request
		append([]byte{1, 2}, "HTTP/1.1 200 OK\r\n"...),
		append([]byte{1, 4}, ok...), // answers before the request
		append([]byte{1, 0}, strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", 20)...),
		append([]byte{1, 0}, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\nConnection: Upgrade\r\n\r\n"...),
		append([]byte{1, 0}, "HTTP/1.1 200 OK\r\nX: "+strings.Repeat("a", 2<<20)...),
		{1, 0, 0, 1, 2, 3, 0xff},
		{1, 1},
		// Raw bytes on the session (mode 2).
		cat([]byte{2}, frame(0, 0, 1, 2, 0)),
		cat([]byte{2}, frame(0, 1, 0, 1, 0x7fffffff), frame(0, 1, 0, 1, 0x7fffffff)),
		cat([]byte{2}, frame(0, 0, 0, 1, 1<<20, bytes.Repeat([]byte{'x'}, 100)...)),
		{2, 0xff, 0xff, 0xff},
		// Garbage before TLS (mode 3).
		{3, 0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'},
		{3, 0x16, 0x03, 0x03, 0xff, 0xff},
		{3},
		// Identities: the P-256 key, another member's name, a key nobody
		// registered.
		{1 << 2, 0x02, 0x01, 0, 1, 2, 3, 4}, append([]byte{1<<2 | 1, 0}, ok...),
		{2 << 2}, {3 << 2}, {2<<2 | 1, 0}, {3 << 2, 0, 1, 1, 2, 0},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		rig.run(t, data)
	})
}
