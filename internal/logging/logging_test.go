package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// What must never reach a log, passed the ways callers pass things.
func TestRedact(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, Flags{Level: "debug"}, "web")
	if err != nil {
		t.Fatal(err)
	}
	const pat = "glpat-abcdefghijklmnopqrstuvwxyz"
	l.Info("a", "token", "plain-but-named", "session_cookie", "x", "Authorization", "Bearer abc", "client_secret_file", "/s",
		"url", "https://gitlab.example.com/auth/callback/abc?code=c0de&state=st4te", "note", "the token is "+pat)
	l.Warn("b", "err", errors.New(`Get "https://gitlab.example.com/api/v4/user?private_token=`+pat+`": EOF`), "paths", []string{"a?x", "https://h/p?q=1"})
	out := buf.String()
	for _, leak := range []string{"plain-but-named", "Bearer abc", "c0de", "st4te", pat, "q=1", "/s\""} {
		if strings.Contains(out, leak) {
			t.Errorf("the log carries %q:\n%s", leak, out)
		}
	}
	for _, kept := range []string{`"role":"web"`, "https://gitlab.example.com/auth/callback/abc?[redacted]", `"msg":"a"`, "the token is"} {
		if !strings.Contains(out, kept) {
			t.Errorf("the log lacks %q:\n%s", kept, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("not one JSON object per line: %q", line)
		}
	}
}

func TestLevelsAndFormats(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, Flags{Level: "warn", Format: "text"}, "")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("quiet")
	l.Warn("loud", "n", 1)
	if out := buf.String(); strings.Contains(out, "quiet") || !strings.Contains(out, "level=WARN msg=loud n=1") {
		t.Errorf("text at warn: %q", out)
	}
	for _, bad := range []Flags{{Level: "verbose"}, {Format: "xml"}} {
		if _, err := New(&buf, bad, ""); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var f Flags
	f.Register(fs)
	if err := fs.Parse([]string{"-log-level", "debug", "-log-format", "text"}); err != nil || f.Level != "debug" || f.Format != "text" {
		t.Errorf("flags %+v %v", f, err)
	}
}

// A repeating warning: the first at once, then once a period with how
// many were held back.
func TestEvery(t *testing.T) {
	now := time.Unix(0, 0)
	e := &Every{Period: time.Minute, now: func() time.Time { return now }}
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	for i := 0; i < 5; i++ {
		e.Warn(l, "daemon-a", "daemon not reached")
		now = now.Add(10 * time.Second)
	}
	e.Warn(l, "daemon-b", "daemon not reached")
	now = now.Add(time.Minute)
	e.Warn(l, "daemon-a", "daemon not reached")
	out := buf.String()
	if strings.Count(out, "daemon not reached") != 3 || !strings.Contains(out, "suppressed=4") {
		t.Errorf("every minute: %q", out)
	}
}
