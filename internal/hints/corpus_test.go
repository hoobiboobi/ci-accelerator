package hints

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCorpus reads a folder of real Dockerfiles (HINTS_CORPUS=<folder>)
// and writes, for every RUN step the hints have something to say about,
// what they say (HINTS_CORPUS_OUT, or the test log): for reading by hand,
// to find what the rules get wrong on Dockerfiles nobody wrote for them.
// A Dockerfile is turned into steps the way the frontend records them as
// far as the hints read them: the shell command, the stage's ENV and ARG
// defaults, USER, WORKDIR and RUN's cache mounts, plus the variables some
// official base images set. Skipped without the variable.
func TestCorpus(t *testing.T) {
	dir := os.Getenv("HINTS_CORPUS")
	if dir == "" {
		t.Skip("HINTS_CORPUS not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	sort.Strings(files)
	var out strings.Builder
	steps, hinted := 0, 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range readDockerfile(string(data)) {
			steps++
			hs := Analyze(s.op, s.ancestors)
			if len(hs) == 0 {
				continue
			}
			hinted++
			cmd := strings.Join(strings.Fields(s.cmd), " ")
			if len(cmd) > 160 {
				cmd = cmd[:160] + "…"
			}
			fmt.Fprintf(&out, "%s:%d  %s\n    %s\n", filepath.Base(f), s.line, cmd, strings.Join(summary(hs), " | "))
		}
	}
	fmt.Fprintf(&out, "%d files, %d RUN steps, %d with hints\n", len(files), steps, hinted)
	if p := os.Getenv("HINTS_CORPUS_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Log(out.String())
}

type dfStep struct {
	line      int
	cmd       string
	op        json.RawMessage
	ancestors []json.RawMessage
}

type dfStage struct {
	env       map[string]string
	user, cwd string
	shell     []string
	ops       []json.RawMessage
}

// imageEnv are variables official images set that place caches.
var imageEnv = map[string][]string{
	"golang": {"GOPATH=/go"}, "rust": {"CARGO_HOME=/usr/local/cargo", "RUSTUP_HOME=/usr/local/rustup"},
	"ruby": {"GEM_HOME=/usr/local/bundle", "BUNDLE_APP_CONFIG=/usr/local/bundle"}, "composer": {"COMPOSER_HOME=/tmp"},
	"denoland/deno": {"DENO_DIR=/deno-dir/"}, "mambaorg/micromamba": {"MAMBA_ROOT_PREFIX=/opt/conda"},
}

func readDockerfile(text string) []dfStep {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	stages := map[string]*dfStage{}
	var cur *dfStage
	var out []dfStep
	for i := 0; i < len(lines); i++ {
		start := i + 1
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		for strings.HasSuffix(l, "\\") && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			if strings.HasPrefix(next, "#") {
				continue
			}
			l = strings.TrimSuffix(l, "\\") + " " + next
		}
		word, rest, _ := strings.Cut(l, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToUpper(word) {
		case "FROM":
			fs := strings.Fields(rest)
			for len(fs) > 0 && strings.HasPrefix(fs[0], "--") {
				fs = fs[1:]
			}
			if len(fs) == 0 {
				continue
			}
			img := fs[0]
			s := &dfStage{env: map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, cwd: "/", shell: []string{"/bin/sh", "-c"}}
			if base, ok := stages[strings.ToLower(img)]; ok {
				s.env = merge(base.env, nil)
				s.user, s.cwd, s.shell = base.user, base.cwd, base.shell
				s.ops = append([]json.RawMessage(nil), base.ops...)
			} else {
				name := img
				if j := strings.LastIndex(name, "@"); j >= 0 {
					name = name[:j]
				}
				if j := strings.LastIndex(name, ":"); j > strings.LastIndex(name, "/") {
					name = name[:j]
				}
				name = strings.TrimPrefix(strings.TrimPrefix(name, "docker.io/"), "library/")
				for _, kv := range imageEnv[name] {
					k, v, _ := strings.Cut(kv, "=")
					s.env[k] = v
				}
			}
			if len(fs) >= 3 && strings.EqualFold(fs[1], "AS") {
				stages[strings.ToLower(fs[2])] = s
			}
			cur = s
		case "ENV":
			if cur == nil {
				continue
			}
			if !strings.Contains(strings.Fields(rest + " x")[0], "=") {
				k, v, _ := strings.Cut(rest, " ")
				cur.env[k] = expand(strings.Trim(strings.TrimSpace(v), `"`), cur.env)
				continue
			}
			for _, w := range split(rest) {
				for _, kv := range w {
					if k, v, ok := strings.Cut(kv, "="); ok {
						cur.env[k] = expand(v, cur.env)
					}
				}
			}
		case "ARG":
			if cur == nil {
				continue
			}
			for _, kv := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(kv, "="); ok {
					cur.env[k] = strings.Trim(v, `"`)
				}
			}
		case "USER":
			if cur != nil {
				cur.user = expand(rest, cur.env)
			}
		case "WORKDIR":
			if cur != nil {
				d := expand(strings.Trim(rest, `"`), cur.env)
				if !strings.HasPrefix(d, "/") {
					d = path.Join(cur.cwd, d)
				}
				cur.cwd = path.Clean(d)
			}
		case "SHELL":
			var sh []string
			if cur != nil && json.Unmarshal([]byte(rest), &sh) == nil {
				cur.shell = sh
			}
		case "RUN":
			if cur == nil {
				continue
			}
			var mounts []map[string]any
			mounts = append(mounts, map[string]any{"dest": "/"})
			for strings.HasPrefix(rest, "--") {
				flag, r, _ := strings.Cut(rest, " ")
				rest = strings.TrimSpace(r)
				opts, ok := strings.CutPrefix(flag, "--mount=")
				if !ok {
					continue
				}
				m := map[string]any{}
				cache := false
				sharing := 0
				for _, kv := range strings.Split(opts, ",") {
					k, v, _ := strings.Cut(kv, "=")
					switch k {
					case "type":
						cache = v == "cache"
					case "target", "dst", "destination":
						if !strings.HasPrefix(v, "/") {
							v = path.Join(cur.cwd, v)
						}
						m["dest"] = path.Clean(expand(v, cur.env))
					case "sharing":
						sharing = map[string]int{"shared": 0, "private": 1, "locked": 2}[v]
					}
				}
				if cache {
					m["mountType"] = 3
					m["cacheOpt"] = map[string]any{"ID": m["dest"], "sharing": sharing}
				}
				mounts = append(mounts, m)
			}
			cmd := rest
			if strings.HasPrefix(cmd, "<<") {
				// A heredoc: the lines up to its end word.
				end := strings.Trim(strings.Fields(strings.TrimPrefix(cmd, "<<"))[0], `"'-`)
				var body []string
				for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != end {
					i++
					body = append(body, lines[i])
				}
				i++
				cmd = strings.Join(body, "\n")
			}
			var args []string
			if strings.HasPrefix(cmd, "[") && json.Unmarshal([]byte(cmd), &args) == nil {
			} else {
				args = append(append([]string(nil), cur.shell...), cmd)
			}
			var env []string
			for k, v := range cur.env {
				env = append(env, k+"="+v)
			}
			sort.Strings(env)
			meta := map[string]any{"args": args, "env": env, "cwd": cur.cwd}
			if cur.user != "" {
				meta["user"] = cur.user
			}
			op, _ := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": meta, "mounts": mounts}}})
			out = append(out, dfStep{line: start, cmd: cmd, op: op, ancestors: append([]json.RawMessage(nil), cur.ops...)})
			cur.ops = append(cur.ops, op)
		}
	}
	return out
}
