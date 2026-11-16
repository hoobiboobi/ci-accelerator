package diskprobe

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A build step can reach the probe (it shares the daemon pod's network):
// the connections it holds open are bounded, the next one waits until one
// of them closes.
func TestProbeBoundsItsConnections(t *testing.T) {
	maxConns = 2
	defer func() { maxConns = 32 }()
	logs := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", t.TempDir(), nil, slog.New(slog.NewJSONHandler(logs, nil)))
	}()
	defer func() { cancel(); <-done }()
	re := regexp.MustCompile(`"listen":"(127\.0\.0\.1:\d+)"`)
	addr := ""
	for deadline := time.Now().Add(5 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %s", logs.String())
		}
	}
	var idle []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		idle = append(idle, c)
	}
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("tcp", addr) // the kernel takes it; the probe does not yet
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := http.ReadResponse(br, nil); err == nil {
		t.Fatal("a connection over the bound was served while the others stayed open")
	}
	idle[0].Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("not served once a connection closed: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %s", res.Status)
	}
}
