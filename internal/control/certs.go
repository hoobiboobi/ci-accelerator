package control

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// ReloadingCert serves the certificate in certFile and keyFile, read again
// when either file changes (a renewed Secret is updated in place in the
// pod): the controller never needs a restart for a new certificate.
func ReloadingCert(certFile, keyFile string) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	var mu sync.Mutex
	var cur *tls.Certificate
	var stamp time.Time
	var checked time.Time
	load := func() error {
		cs, err := os.Stat(certFile)
		if err != nil {
			return err
		}
		ks, err := os.Stat(keyFile)
		if err != nil {
			return err
		}
		m := cs.ModTime()
		if ks.ModTime().After(m) {
			m = ks.ModTime()
		}
		if cur != nil && m.Equal(stamp) {
			return nil
		}
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			if cur != nil {
				return nil // half-written: keep the old one, try again later
			}
			return err
		}
		cur, stamp = &c, m
		return nil
	}
	if err := load(); err != nil {
		return nil, err
	}
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(checked) > 10*time.Second {
			checked = time.Now()
			load()
		}
		return cur, nil
	}, nil
}
