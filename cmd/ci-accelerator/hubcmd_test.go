package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// The hub's commands as the docs spell them, also with -data after the
// arguments, and their help.
func TestHubCommands(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	reg, err := fleet.OpenRegistry(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()
	const who = "gitlab:gitlab.example.com/1234567"
	if err := hubCommand("grant", []string{"owner", who, "-data", data}); err != nil {
		t.Fatalf("grant, -data after the arguments: %v", err)
	}
	if err := hubCommand("grant", []string{"-data", data, "viewer", "gitlab:gitlab.example.com/7"}); err != nil {
		t.Fatalf("grant, -data first: %v", err)
	}
	reg, err = fleet.OpenRegistry(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if r := reg.RoleOf(who); r != fleet.Owner {
		t.Errorf("after grant: %q", r)
	}
	reg.Close()
	if err := hubCommand("grant", []string{"-data", data, "owner"}); err == nil {
		t.Error("grant without a subject: no error")
	}

	backup := filepath.Join(t.TempDir(), "fleet-backup.db")
	if err := hubCommand("backup", []string{backup, "-data", data}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("no backup file: %v", err)
	}
	other := t.TempDir()
	if err := hubCommand("restore", []string{"-data", other, backup}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, applied, err := fleet.ApplyRestore(other, time.Now()); err != nil || !applied {
		t.Fatalf("the staged restore: applied %v, %v", applied, err)
	}
	reg, err = fleet.OpenRegistry(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if r := reg.RoleOf(who); r != fleet.Owner {
		t.Errorf("after the restore: %q", r)
	}

	if err := hubCommand("help", nil); err != nil {
		t.Errorf("help: %v", err)
	}
	if err := hubCommand("frobnicate", nil); err == nil {
		t.Error("an unknown command: no error")
	}
}

// stdoutOf runs f and returns what it printed.
func stdoutOf(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	ferr := f()
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	if ferr != nil {
		t.Fatal(ferr)
	}
	return string(out)
}

// backup and restore tell the operator what the file holds and what a
// restore brings back.
func TestHubBackupRestoreMessages(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	reg, err := fleet.OpenRegistry(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()
	backup := filepath.Join(t.TempDir(), "fleet-backup.db")
	out := stdoutOf(t, func() error { return hubCommand("backup", []string{"-data", data, backup}) })
	for _, want := range []string{"sealed secrets", "readable by the hub's user only", "delete it here once copied out"} {
		if !strings.Contains(out, want) {
			t.Errorf("backup's message misses %q: %s", want, out)
		}
	}
	out = stdoutOf(t, func() error { return hubCommand("restore", []string{"-data", t.TempDir(), backup}) })
	for _, want := range []string{"return as they were in the backup", "review Members and Access", "every API and join token is voided"} {
		if !strings.Contains(out, want) {
			t.Errorf("restore's message misses %q: %s", want, out)
		}
	}
}
