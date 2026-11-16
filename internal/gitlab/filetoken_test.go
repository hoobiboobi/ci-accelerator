package gitlab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A token file is read at every use: missing at first (a fleet hub has not
// delivered it yet), then there, then replaced.
func TestFileToken(t *testing.T) {
	ctx := context.Background()
	f := filepath.Join(t.TempDir(), "token")
	tok := FileToken(f)
	if _, err := tok.Token(ctx); err == nil {
		t.Fatal("a missing file gives a token")
	}
	os.WriteFile(f, []byte("  \n"), 0o600)
	if _, err := tok.Token(ctx); err == nil {
		t.Error("an empty file gives a token")
	}
	os.WriteFile(f, []byte("glpat-one\n"), 0o600)
	if v, err := tok.Token(ctx); err != nil || v != "glpat-one" {
		t.Errorf("token %q %v", v, err)
	}
	if _, err := tok.Renew(ctx, "glpat-one"); !errors.Is(err, ErrRefused) {
		t.Errorf("renew of the same token: %v", err)
	}
	os.WriteFile(f, []byte("glpat-two"), 0o600)
	if v, err := tok.Renew(ctx, "glpat-one"); err != nil || v != "glpat-two" {
		t.Errorf("renew after a replacement: %q %v", v, err)
	}
}
