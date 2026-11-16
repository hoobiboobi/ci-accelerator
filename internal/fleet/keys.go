// Package fleet joins several boxes under one console: a hub, and members
// (each a box's controller) that dial out to it.
//
// A member keeps one outgoing WebSocket to the hub's console address. Over
// it runs one TLS 1.3 session in which each side proves a key the other
// has pinned (no certificate authority; the hub confirms the member's key
// with one byte, since in TLS 1.3 the client's handshake ends before the
// server has judged it), and inside that a multiplexer: the hub opens a
// stream per request and the member answers each with its controller's
// own API. Members behind NAT need no inbound address.
package fleet

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Keys are PEM in the forms standard tools write: a private key as PKCS#8
// ("PRIVATE KEY", what `openssl genpkey` and Terraform's tls provider write)
// or SEC1 ("EC PRIVATE KEY"); public keys as PKIX ("PUBLIC KEY"). Ed25519
// and ECDSA on P-256 or P-384.

// ParsePrivateKey reads the first private key in b.
func ParsePrivateKey(b []byte) (crypto.Signer, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("no PEM private key (PRIVATE KEY or EC PRIVATE KEY)")
		}
		switch blk.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, err
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("not a signing key")
			}
			return s, checkPublic(s.Public())
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(blk.Bytes)
			if err != nil {
				return nil, err
			}
			return k, checkPublic(k.Public())
		}
	}
}

// ParsePublicKeys reads every public key in b; at least one.
func ParsePublicKeys(b []byte) ([]crypto.PublicKey, error) {
	var out []crypto.PublicKey
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "PUBLIC KEY" {
			continue
		}
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		if err := checkPublic(k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("no PEM public key (PUBLIC KEY)")
	}
	return out, nil
}

func checkPublic(k crypto.PublicKey) error {
	switch p := k.(type) {
	case ed25519.PublicKey:
		return nil
	case *ecdsa.PublicKey:
		if p.Curve == elliptic.P256() || p.Curve == elliptic.P384() {
			return nil
		}
		return fmt.Errorf("ECDSA on %s: use P-256, P-384 or Ed25519", p.Curve.Params().Name)
	}
	return fmt.Errorf("a %T key: use Ed25519 or ECDSA on P-256 or P-384", k)
}

// Fingerprint names a public key the way OpenSSH does: SHA256 of its
// PKIX form, base64 without padding.
func Fingerprint(k crypto.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// PublicPEM is k in PKIX PEM.
func PublicPEM(k crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// GenerateKey makes an Ed25519 key: the private key as PKCS#8 PEM and the
// public key as PKIX PEM.
func GenerateKey() (private, public []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	public, err = PublicPEM(pub)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), public, err
}

// certificate wraps a key in a self-signed certificate: TLS needs one, and
// only the key in it counts (the peer pins the key, not the certificate).
func certificate(name string, key crypto.Signer) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// pinned is a TLS configuration that accepts exactly the peers whose key
// accept says yes to (by fingerprint); certificate authorities and names
// play no part.
func pinned(own tls.Certificate, server bool, accept func(fingerprint string) error) *tls.Config {
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no key shown")
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if err := checkPublic(c.PublicKey); err != nil {
			return err
		}
		return accept(Fingerprint(c.PublicKey))
	}
	conf := &tls.Config{Certificates: []tls.Certificate{own}, MinVersion: tls.VersionTLS13,
		InsecureSkipVerify: true, VerifyPeerCertificate: verify, ServerName: "fleet"}
	if server {
		conf.ClientAuth = tls.RequireAnyClientCert
	}
	return conf
}
