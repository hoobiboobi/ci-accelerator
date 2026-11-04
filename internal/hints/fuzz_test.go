package hints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzAnalyze feeds arbitrary scripts, environments, users, working
// folders and mounts: no panic, the same answer twice, and a fragment
// that is a RUN (after the lines it adds before it) whenever there is one.
func FuzzAnalyze(f *testing.F) {
	for _, s := range []string{
		"npm ci", "apt-get update && apt-get install -y curl", "pip install --no-cache-dir -r r.txt",
		`sh -c "cd /x && npm ci"`, "export GOPATH=/go; go build ./...", "cargo build && cp target/release/a /a",
		"a='b && c' ; (npm i) | tee x", "env -i A=1 sudo -u x npm ci", "${A:-npm} ci", "$(npm ci) `pip install`",
		"poetry install", "\\\nnpm \\\n ci", "# npm ci\nnpm ci", "npx --yes pnpm install", "dnf -y install x && dnf clean all",
	} {
		f.Add(s, "HOME=/root\nGOPATH=/go", "", "/app", "/root/.npm:2,/app/node_modules:0", "RUN --mount=type=cache,target=/x "+s)
	}
	f.Fuzz(func(t *testing.T, cmd, env, user, cwd, mounts, name string) {
		var ms []map[string]any
		for _, m := range strings.Split(mounts, ",") {
			dest, sharing, _ := strings.Cut(m, ":")
			ms = append(ms, map[string]any{"dest": dest, "mountType": 3, "cacheOpt": map[string]any{"ID": dest, "sharing": len(sharing)}})
		}
		b, err := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{
			"meta":   map[string]any{"args": []string{"/bin/sh", "-c", cmd}, "env": strings.Split(env, "\n"), "user": user, "cwd": cwd},
			"mounts": ms,
		}}})
		if err != nil {
			t.Skip()
		}
		anc, _ := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"/bin/sh", "-c", name}}}}})
		a := Analyze(b, []json.RawMessage{anc})
		if again := Analyze(b, []json.RawMessage{anc}); !reflect.DeepEqual(a, again) {
			t.Fatalf("not deterministic: %v vs %v", a, again)
		}
		for _, h := range a {
			if len(h.Problems) == 0 || len(h.Caches) == 0 {
				t.Fatalf("hint without problems or caches: %+v", h)
			}
		}
		frag := Fragment("RUN "+cmd, a)
		if len(a) > 0 && frag == "" {
			t.Fatalf("no fragment for a RUN with hints")
		}
		if frag != "" {
			lines := strings.Split(frag, "\n")
			found := false
			for _, l := range lines {
				if strings.HasPrefix(l, "RUN ") || l == "RUN" {
					found = true
				}
			}
			if !found {
				t.Fatalf("fragment without a RUN: %q", frag)
			}
		}
		_ = Fragment(name, a)
	})
}
