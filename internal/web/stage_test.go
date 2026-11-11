package web

import "testing"

// A step's stage on the pages: its key's name without the padding BuildKit
// gives the number, "build" for a Dockerfile without stage names.
func TestStageOf(t *testing.T) {
	for key, want := range map[string]string{
		"[ 3/10]":                     "build",
		"[3/4]":                       "build",
		"[dev-base  2/12]":            "dev-base",
		"[linux/amd64 builder  2/14]": "linux/amd64 builder",
	} {
		if got := stageOf(key); got != want {
			t.Errorf("stageOf(%q) = %q, want %q", key, got, want)
		}
	}
}
