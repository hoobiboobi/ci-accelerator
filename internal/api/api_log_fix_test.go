package api

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// The API logs through the role's logger: JSON with a level, like every
// other line of the console.
func TestLogsThroughTheRoleLogger(t *testing.T) {
	var buf strings.Builder
	get(t, Handler(&failingStore{}, slog.New(slog.NewJSONHandler(&buf, nil))), "/api/v1/builds")
	line := strings.TrimSpace(buf.String())
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil || v["level"] != "ERROR" || !strings.Contains(line, "connection refused") {
		t.Errorf("log line %q (%v)", line, err)
	}
}
