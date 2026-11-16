package main

import (
	"os"
	"strings"
	"testing"
)

// The chart passes the store's bound in the environment (an older image
// still starts); the flag takes it from there and refuses a negative one.
func TestStoreBoundFromTheEnvironment(t *testing.T) {
	t.Setenv(maxStoreEnv, "-5")
	var buf lockedBuf
	logOut = &buf
	defer func() { logOut = os.Stderr }()
	err := controllerMain([]string{"-data", t.TempDir(), "-daemon", "tcp://127.0.0.1:1", "-gitlab-url", "https://gitlab.example.com/"}, false)
	if err == nil || !strings.Contains(err.Error(), "-max-store-mib must not be negative") {
		t.Errorf("a negative bound from $%s: %v", maxStoreEnv, err)
	}
}
