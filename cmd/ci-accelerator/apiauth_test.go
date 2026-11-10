package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
)

// Who may call the controller's API: a static token from a file (outside
// a cluster), else the listed ServiceAccounts, each with one role. A
// mistake in either refuses to start instead of opening the API.
func TestAPIAuthorizer(t *testing.T) {
	dir := t.TempDir()
	file := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	authorize, err := apiAuthorizer(config{apiTokenFile: file("token", "  a-token-of-24-characters \n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(header string) (control.Role, error) {
		r := httptest.NewRequest("POST", "/internal/v1/call/Ping", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		return authorize(r)
	}
	if role, err := call("Bearer a-token-of-24-characters"); err != nil || role != control.RoleAny {
		t.Errorf("the file's token (trimmed): %q %v", role, err)
	}
	for _, h := range []string{"", "Bearer ", "Bearer a-token-of-24-characterz", "Bearer a-token-of-24-characters-and-more", "a-token-of-24-characters"} {
		if _, err := call(h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}

	for _, c := range []struct {
		name string
		c    config
		want string
	}{
		{"missing token file", config{apiTokenFile: filepath.Join(dir, "none")}, "no such file"},
		{"short token", config{apiTokenFile: file("short", "15-characters!!\n")}, "at least 16"},
		{"no namespace", config{apiAllow: []string{"box-web"}}, "namespace/name"},
		{"empty name", config{apiAllow: []string{"box/"}}, "namespace/name"},
		{"empty namespace", config{apiAllowRouter: []string{"/box-router"}}, "namespace/name"},
		{"one account, two roles", config{apiAllow: []string{"box/sa"}, apiAllowRouter: []string{"box/sa"}}, "one role"},
	} {
		if _, err := apiAuthorizer(c.c, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", c.name, err, c.want)
		}
	}
	if a, err := apiAuthorizer(config{apiAllow: []string{"box/box-web"}, apiAllowRouter: []string{"box/box-router"}}, nil); err != nil || a == nil {
		t.Errorf("ServiceAccounts: %v", err)
	}
}
