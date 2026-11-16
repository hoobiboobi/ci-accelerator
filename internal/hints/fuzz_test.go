package hints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzAnalyze feeds arbitrary scripts, environments, users, working
// folders and mounts: no panic (analyze, without Analyze's recover), the
// same answer twice, and a fragment that is a RUN (after the lines it adds
// before it) whenever there is one, except for a here-document.
func FuzzAnalyze(f *testing.F) {
	for _, s := range []string{
		"npm ci", "apt-get update && apt-get install -y curl", "pip install --no-cache-dir -r r.txt",
		`sh -c "cd /x && npm ci"`, "export GOPATH=/go; go build ./...", "cargo build && cp target/release/a /a",
		"a='b && c' ; (npm i) | tee x", "env -i A=1 sudo -u x npm ci", "${A:-npm} ci", "$(npm ci) `pip install`",
		"poetry install", "\\\nnpm \\\n ci", "# npm ci\nnpm ci", "npx --yes pnpm install", "dnf -y install x && dnf clean all",
		"exit 0\nA=aaaaaaaaaaaaaaaa\n" + strings.Repeat("A=$A$A\n", 40) + "npm ci",
		"apt-get install -y x && apt-get clean && rm -rf /var/lib/apt/lists/*", "uv sync -n && head -n 5 x",
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
		a := analyze(b, []json.RawMessage{anc})
		if again := analyze(b, []json.RawMessage{anc}); !reflect.DeepEqual(a, again) {
			t.Fatalf("not deterministic: %v vs %v", a, again)
		}
		for _, h := range a {
			if len(h.Problems) == 0 || len(h.Caches) == 0 {
				t.Fatalf("hint without problems or caches: %+v", h)
			}
		}
		frag := Fragment("RUN "+cmd, a)
		if len(a) > 0 && frag == "" && !strings.Contains(cmd, "<<") {
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

// FuzzInstalls feeds the order hint's path: no panic (installs, without
// Installs' recover), the same answer twice, and the texts it shows (the
// other commands, the fragment's RUN lines) taken from the step's name,
// which the console masks, never from the operation.
func FuzzInstalls(f *testing.F) {
	for _, s := range []string{
		"pip install --no-cache-dir .", "pip install -e .", "uv pip install --system -e .", "pip install -rrequirements.txt",
		"npm ci --prefix web && npm run embed --prefix web", "composer install", "uv sync --locked", "go mod download",
		"pip install --extra-index-url https://u:secret@x/simple . && curl -H 'Authorization: Bearer secret' x",
		"cd web && npm ci", "poetry config a b && poetry install", "npm ci; npm run build",
	} {
		f.Add(s, "RUN "+s, "PYTHON_VERSION=3.12.1", "/app", "COPY . .", "requirements/base.txt")
		f.Add(s, "RUN "+strings.ReplaceAll(s, "secret", "[masked]"), "PYTHON_VERSION=3.12.1", "/app", "COPY --chown=a . /app", "src/x.py")
	}
	f.Fuzz(func(t *testing.T, script, name, env, cwd, copy, file string) {
		b, err := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{
			"meta": map[string]any{"args": []string{"/bin/sh", "-c", script}, "env": strings.Split(env, "\n"), "cwd": cwd},
		}}})
		if err != nil {
			t.Skip()
		}
		s := installs(b, name)
		if again := installs(b, name); !reflect.DeepEqual(s, again) {
			t.Fatalf("not deterministic")
		}
		_ = WholeContext(copy)
		if s == nil {
			return
		}
		_ = s.Managers()
		_ = s.Matters(file)
		shown := strings.ReplaceAll(name, "\\\n", " ")
		for _, o := range s.Others {
			if !strings.Contains(shown, o) {
				t.Fatalf("other command %q is not in the name %q", o, name)
			}
		}
		for _, parents := range []bool{true, false} {
			frag := s.OrderFragment(copy, parents)
			if frag != "" && !strings.Contains(frag, copy) {
				t.Fatalf("fragment without the copy %q:\n%s", copy, frag)
			}
		}
	})
}
