package control

import (
	"crypto/tls"
	"net/http"
	"time"
)

// Transport is the HTTP transport of the web and the routers to the
// controller's API: TLS with conf, and HTTP/2 pings, so a controller whose
// node vanished without closing the connection is noticed within about 20
// seconds, not at each call's timeout (or the minutes the kernel spends
// retransmitting) and not never on the change stream, which only waits.
func Transport(conf *tls.Config) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = conf
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 5 * time.Second}
	return tr
}
