package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A member's strings lose control characters and bidirectional overrides
// before the hub's pages see them; tabs, newlines and other text stay.
func TestMemberStringsAreCleaned(t *testing.T) {
	type inner struct {
		Name string
		Tags []string
		By   map[string]string
		Any  any
		Data []byte
	}
	v := struct {
		Title string
		P     *inner
		When  time.Time
		Pipe  store.Pipeline
	}{
		Title: "fix\u202egnp.exe\x1b[31m red\tand\nmore é",
		P: &inner{Name: "ok\x00\x07", Tags: []string{"a\u2066b"}, By: map[string]string{"k": "v\u200f"},
			Any: "x\x1by", Data: []byte{0x1b, 0x00}},
		Pipe: store.Pipeline{CommitTitle: "t\u202dz", ProjectPath: "g/\x7fp"},
	}
	cleanStrings(&v)
	for got, want := range map[string]string{
		v.Title: "fixgnp.exe[31m red\tand\nmore é", v.P.Name: "ok", v.P.Tags[0]: "ab", v.P.By["k"]: "v",
		v.P.Any.(string): "xy", v.Pipe.CommitTitle: "tz", v.Pipe.ProjectPath: "g/p",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	if string(v.P.Data) != "\x1b\x00" {
		t.Errorf("bytes changed: %q", v.P.Data)
	}
}

// Map keys are a member's text too (target and daemon names): they are
// cleaned, and keys that become equal keep one entry, the same one
// whatever the map's order.
func TestMemberMapKeysAreCleaned(t *testing.T) {
	type key struct{ Name string }
	for range 20 { // map order differs between runs
		v := struct {
			Targets map[string][]string
			Nested  map[string]map[string]string
			ByKey   map[key]int
			Clash   map[string]int
		}{
			Targets: map[string][]string{"\u202ekey": {"\x07v"}, "ok": {"w"}},
			Nested:  map[string]map[string]string{"a\x1b": {"b\u2066": "c\x00"}},
			ByKey:   map[key]int{{"n\u200e"}: 1},
			Clash:   map[string]int{"x": 1, "\x07x": 2, "x\u202e": 3, "\x00y": 4, "y\x1b": 5},
		}
		cleanStrings(&v)
		for what, ok := range map[string]bool{
			"targets":     reflect.DeepEqual(v.Targets, map[string][]string{"key": {"v"}, "ok": {"w"}}),
			"nested":      reflect.DeepEqual(v.Nested, map[string]map[string]string{"a": {"b": "c"}}),
			"struct keys": reflect.DeepEqual(v.ByKey, map[key]int{{"n"}: 1}),
			// "x" was clean already and stays; of "\x00y" and "y\x1b" the
			// smaller original key wins.
			"clashing keys": reflect.DeepEqual(v.Clash, map[string]int{"x": 1, "y": 4}),
		} {
			if !ok {
				t.Fatalf("%s: %#v", what, v)
			}
		}
	}
}

// What a member says in an error (a wire error's message, the body of a
// failing status, a decoder error quoting a map key) is cleaned; the
// error still is what it wraps.
func TestMemberErrorsAreCleaned(t *testing.T) {
	answers := map[string]struct {
		code int
		body string
	}{
		"/internal/v1/config":            {200, `{"Known":["0a1b2c"]}`},
		"/internal/v1/call/PipelineInfo": {200, `{"r":[null],"err":{"msg":"\u202edenied\u001b[0m","kind":"denied"}}`},
		"/internal/v1/call/Projects":     {500, "broken \x1b[31m\u202e"},
		"/internal/v1/call/BuildHints":   {200, `{"r":[{"1\u202e":[]}]}`},
		"/internal/v1/call/Ping":         {200, `{"r":[],"err":{"msg":"gone\u0007","kind":"notfound"}}`},
	}
	var cfgFails atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/internal/v1/config" && cfgFails.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("cfg \x1b[2J\u202ebad"))
			return
		}
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client(), stream: srv.Client(), member: true, reads: map[string]readAnswer{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	if _, err := c.config(ctx); err != nil {
		t.Fatal(err)
	}
	dirty := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		if s := err.Error(); cleanString(s) != s {
			t.Errorf("%s: the error holds what the hub drops: %q", what, s)
		}
	}
	_, err := c.PipelineInfo(ctx, "0a1b2c", 1)
	dirty("wire error", err)
	if !errors.Is(err, auth.ErrDenied) {
		t.Errorf("wire error no longer wraps auth.ErrDenied: %v", err)
	}
	_, err = c.Projects(ctx)
	dirty("failing status", err)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("failing status no longer wraps ErrUnavailable: %v", err)
	}
	_, err = c.BuildHints(ctx, []int64{1})
	dirty("decoder error", err)
	err = c.Ping(ctx)
	dirty("plain wire error", err)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wire error no longer wraps store.ErrNotFound: %v", err)
	}
	cfgFails.Store(true)
	_, err = c.config(ctx)
	dirty("configuration status", err)
}
