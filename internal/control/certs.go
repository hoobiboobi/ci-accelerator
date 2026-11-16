package control

import (
	"crypto/tls"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/logging"
)

// How often the files are looked at, how long a reload may fail before it
// is reported (a pair caught half-written is read again 10 s later), and
// how often it is reported again while it keeps failing.
var (
	certCheckEvery = 10 * time.Second
	certWarnAfter  = time.Minute
	certWarnEvery  = 10 * time.Minute
)

// ReloadingCert serves the certificate in certFile and keyFile, read again
// when either file changes (a renewed Secret is updated in place in the
// pod): the controller never needs a restart for a new certificate.
func ReloadingCert(certFile, keyFile string) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	return ReloadingCertLog(certFile, keyFile, nil)
}

// ReloadingCertLog is ReloadingCert that warns on log when the files
// cannot be read again (the old certificate is still served); log nil:
// nothing is said.
func ReloadingCertLog(certFile, keyFile string, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	var mu sync.Mutex
	var cur *tls.Certificate
	var stamp time.Time
	var checked, failing time.Time
	warn := logging.Every{Period: certWarnEvery}
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
			return err // the old one, if any, is kept
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
		if now := time.Now(); now.Sub(checked) > certCheckEvery {
			checked = now
			err := load()
			switch {
			case err == nil:
				failing = time.Time{}
			case failing.IsZero():
				failing = now
			case now.Sub(failing) >= certWarnAfter && log != nil:
				warn.Warn(log, certFile, "the controller's certificate cannot be read again; the old one is still served",
					"cert", certFile, "since", failing.UTC().Format(time.RFC3339), "err", err)
			}
		}
		return cur, nil
	}, nil
}
