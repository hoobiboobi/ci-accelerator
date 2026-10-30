package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The kubelet swaps a mounted ConfigMap's files in place: the URLs the
// controller and the routers started with are followed to their new value.
func TestFollowURLsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urls")
	if err := os.WriteFile(path, []byte("https://gitlab.example.com/"), 0o600); err != nil {
		t.Fatal(err)
	}
	was, urls, err := readURLsFile(path)
	if err != nil || len(urls) != 1 || urls[0] != "https://gitlab.example.com/" {
		t.Fatalf("read %q %v", urls, err)
	}
	if _, _, err := readURLsFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file read without an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan []string, 4)
	go followURLsFile(ctx, path, was, 10*time.Millisecond, func(u []string) { changed <- u })
	select {
	case u := <-changed:
		t.Fatalf("changed without a change: %q", u)
	case <-time.After(100 * time.Millisecond):
	}
	replace(t, path, "")
	replace(t, path, "https://gitlab.example.com\nhttps://other.example.com/")
	select {
	case u := <-changed:
		if len(u) != 2 || u[0] != "https://gitlab.example.com" || u[1] != "https://other.example.com/" {
			t.Errorf("new URLs %q", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a changed file was not noticed")
	}
	select {
	case u := <-changed:
		t.Errorf("the same change told twice: %q", u)
	case <-time.After(100 * time.Millisecond):
	}
}

// A new GitLab URL ends the controller (its pod then starts it again):
// the collector, the job watch and GitLab's client keep the URLs they
// started with.
func TestControllerEndsOnNewGitLabURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urls")
	if err := os.WriteFile(path, []byte("https://gitlab.example.com/"), 0o600); err != nil {
		t.Fatal(err)
	}
	urlsEvery = 20 * time.Millisecond
	defer func() { urlsEvery = 30 * time.Second }()
	var buf lockedBuf
	logOut = &buf
	defer func() { logOut = os.Stderr }()
	done := make(chan error, 1)
	go func() {
		done <- controllerMain([]string{"-data", t.TempDir(), "-daemon", "tcp://127.0.0.1:1", "-server-name", "daemons", "-tls-dir", dir,
			"-gitlab-urls-file", path, "-listen", "127.0.0.1:0"}, false)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(buf.String(), `"msg":"ready"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no ready line:\n%s", buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), `"gitlab":["gitlab.example.com"]`) {
		t.Errorf("the URLs of the file are not used:\n%s", buf.String())
	}
	select {
	case err := <-done:
		t.Fatalf("ended without a change: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	replace(t, path, "https://gitlab.example.com")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("still running with the old URLs")
	}
	if !strings.Contains(buf.String(), "the GitLab URLs changed") {
		t.Errorf("no word why it ended:\n%s", buf.String())
	}
}

// replace swaps the file's content at once, as the kubelet does.
func replace(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}
