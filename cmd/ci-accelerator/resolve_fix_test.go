package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// With -discover-all the daemons that are not ready (a stopping one) are
// read too; without that Service (an older BuildKit chart) the ready ones
// still are.
func TestResolveReadsDaemonsThatAreNotReady(t *testing.T) {
	answers := map[string][]string{
		"buildkit-headless.ci.svc": {"10.0.0.2", "10.0.0.1"},
		"buildkit-all.ci.svc":      {"10.0.0.3", "10.0.0.1", "10.0.0.2"},
	}
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		if ips, ok := answers[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	defer func() { lookupHost = lookupHostDefault }()
	log := slog.New(slog.DiscardHandler)
	c := config{discover: "buildkit-headless.ci.svc:1234", discoverAll: "buildkit-all.ci.svc:1234"}
	got, err := resolve(context.Background(), c, log)
	want := []string{"tcp://10.0.0.1:1234", "tcp://10.0.0.2:1234", "tcp://10.0.0.3:1234"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("with the stopping daemon: %q, %v", got, err)
	}
	c.discoverAll = "missing.ci.svc:1234"
	got, err = resolve(context.Background(), c, log)
	if err != nil || !slices.Equal(got, want[:2]) {
		t.Errorf("no Service of every daemon: %q, %v", got, err)
	}
	c.discover = "missing.ci.svc:1234"
	if got, err := resolve(context.Background(), c, log); err == nil {
		t.Errorf("the ready daemons' Service missing: %q", got)
	}
}

var lookupHostDefault = lookupHost

// The checks a changed URL file is held to are the ones a start makes.
func TestURLsRefused(t *testing.T) {
	dir := t.TempDir()
	signIn := filepath.Join(dir, "sign-in.json")
	write := func(text string) {
		if err := os.WriteFile(signIn, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	one, two := []string{"https://a.example.com/"}, []string{"https://a.example.com/", "https://b.example.com/"}
	if err := urlsRefused(config{gitlabTokenFile: "t"}, two); err == nil {
		t.Error("the token next to two GitLabs: accepted")
	}
	if err := urlsRefused(config{gitlabTokenFile: "t"}, one); err != nil {
		t.Errorf("the token next to one GitLab: %v", err)
	}
	write(`{"url":"https://console.example.com","gitlab":[{"clientIdFile":"x","secretFile":"y"}]}`)
	c := config{signIn: signIn}
	if err := urlsRefused(c, one); err != nil {
		t.Errorf("sign-in without url, one GitLab: %v", err)
	}
	if err := urlsRefused(c, two); err == nil {
		t.Error("sign-in without url, two GitLabs: accepted")
	}
	write(`{"url":"https://console.example.com","gitlab":[{"url":"https://a.example.com/","clientIdFile":"x","secretFile":"y"}]}`)
	if err := urlsRefused(c, two); err != nil {
		t.Errorf("sign-in for one of two GitLabs: %v", err)
	}
	if err := urlsRefused(c, []string{"https://b.example.com/"}); err == nil {
		t.Error("sign-in for a GitLab no longer listed: accepted")
	}
}
