package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// The console closes a kept-alive connection no request uses.
func TestConsoleClosesIdleConnections(t *testing.T) {
	idleTimeout = 300 * time.Millisecond
	defer func() { idleTimeout = 2 * time.Minute }()
	addr, stop, done, _ := startAll(t, t.TempDir())
	defer func() { stop(); <-done }()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"))
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = br.ReadByte()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Error("an idle kept-alive connection is still open after 5 s")
	}
}
