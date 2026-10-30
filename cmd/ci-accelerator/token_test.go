package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installation's token is one GitLab's: with several GitLabs the
// console refuses to start rather than send it to each of them.
func TestTokenOfOneGitLab(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("glpat-xxxxxxxxxxxxxxxxxxxx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := controllerMain([]string{"-data", t.TempDir(), "-daemon", "tcp://127.0.0.1:1", "-server-name", "daemons", "-tls-dir", dir,
		"-gitlab-url", "https://gitlab.example.com/", "-gitlab-url", "https://gitlab.other.example/",
		"-gitlab-token-file", token, "-listen", "127.0.0.1:0"}, false)
	if err == nil || !strings.Contains(err.Error(), "token of one GitLab") {
		t.Fatalf("started with one token for two GitLabs: %v", err)
	}
}
