package control

import (
	"crypto/tls"
	"testing"
)

// The connection to the controller is checked with HTTP/2 pings, and the
// TLS settings given are the ones used.
func TestTransportPings(t *testing.T) {
	conf := &tls.Config{ServerName: "c-controller", MinVersion: tls.VersionTLS12}
	tr := Transport(conf)
	if tr.TLSClientConfig != conf || !tr.ForceAttemptHTTP2 {
		t.Errorf("TLS %v, HTTP/2 %v", tr.TLSClientConfig, tr.ForceAttemptHTTP2)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Errorf("no HTTP/2 pings: %+v", tr.HTTP2)
	}
}
