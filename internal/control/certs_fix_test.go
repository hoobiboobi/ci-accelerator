package control

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeNewPair(t *testing.T, certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "controller"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
}

// A renewed pair that cannot be read keeps the old certificate served,
// and says so in the log once it has failed for a while.
func TestCertReloadFailureIsLogged(t *testing.T) {
	defer func(c, w time.Duration) { certCheckEvery, certWarnAfter = c, w }(certCheckEvery, certWarnAfter)
	certCheckEvery, certWarnAfter = 0, 0
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeNewPair(t, cf, kf)
	var buf bytes.Buffer
	get, err := ReloadingCertLog(cf, kf, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := get(nil)
	if first == nil {
		t.Fatal("no certificate")
	}
	os.WriteFile(cf, []byte("not a certificate"), 0o600)
	later := time.Now().Add(time.Minute)
	os.Chtimes(cf, later, later)
	for range 3 {
		if c, _ := get(nil); c != first {
			t.Fatal("the old certificate is not served while the new one cannot be read")
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(buf.String(), "cannot be read again") {
		t.Errorf("nothing logged: %q", buf.String())
	}
	// A good pair again: served, and nothing more is said.
	writeNewPair(t, cf, kf)
	later = later.Add(time.Minute)
	os.Chtimes(cf, later, later)
	buf.Reset()
	if c, _ := get(nil); c == first || c == nil {
		t.Error("the renewed certificate is not served")
	}
	if buf.Len() != 0 {
		t.Errorf("logged after a good reload: %q", buf.String())
	}
}
