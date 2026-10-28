package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A role says when it starts (with its settings), when it is ready, when
// it stops and when it has stopped, each as one JSON object per line.
func TestRolesLogTheirLife(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		role string
		run  func() error
		want []string
	}{
		{"disk-probe", func() error { return diskProbe([]string{"-dir", dir, "-listen", "127.0.0.1:0"}) },
			[]string{`"dir":"` + dir + `"`}},
		{"all", func() error {
			return controllerMain([]string{"-data", t.TempDir(), "-daemon", "tcp://127.0.0.1:1", "-server-name", "daemons", "-tls-dir", dir,
				"-gitlab-url", "https://gitlab.example.com/", "-listen", "127.0.0.1:0", "-log-level", "debug"}, false)
		}, []string{`"gitlab":["gitlab.example.com"]`, `"gitlab_read":"none"`, `"log_level":"debug"`}},
	} {
		t.Run(c.role, func(t *testing.T) {
			var buf lockedBuf
			logOut = &buf
			defer func() { logOut = os.Stderr }()
			done := make(chan error, 1)
			go func() { done <- c.run() }()
			deadline := time.Now().Add(20 * time.Second)
			for !strings.Contains(buf.String(), `"msg":"ready"`) {
				if time.Now().After(deadline) {
					t.Fatalf("no ready line:\n%s", buf.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			syscall.Kill(os.Getpid(), syscall.SIGINT)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("did not stop")
			}
			var msgs []string
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatalf("not a JSON line: %q", line)
				}
				if m["role"] != c.role {
					t.Errorf("a line without its role: %q", line)
				}
				msgs = append(msgs, m["msg"].(string))
			}
			at := map[string]int{}
			for i, m := range msgs {
				if _, ok := at[m]; !ok {
					at[m] = i
				}
			}
			for _, m := range []string{"starting", "ready", "stopping", "stopped"} {
				if _, ok := at[m]; !ok {
					t.Errorf("no %q line: %v", m, msgs)
				}
			}
			if !(at["starting"] < at["ready"] && at["ready"] < at["stopping"] && at["stopping"] < at["stopped"]) {
				t.Errorf("out of order: %v", msgs)
			}
			for _, w := range c.want {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("the start line lacks %s:\n%s", w, buf.String())
				}
			}
		})
	}
}
