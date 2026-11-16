package hints

import (
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// allocated is what f allocates, in bytes.
func allocated(f func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	f()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

// A script that doubles a variable every line ("A=$A$A") is a few hundred
// bytes; reading it must not take memory exponential in its lines. Nor
// may thousands of assignments followed by thousands of commands copy
// every variable into every command without bound.
func TestFixExpansionBounded(t *testing.T) {
	s := "exit 0\nA=aaaaaaaaaaaaaaaa\n" + strings.Repeat("A=$A$A\n", 22) + "npm ci\n"
	if n := allocated(func() { Analyze(op(t, s, nil, "", "/app"), nil) }); n > 64<<20 {
		t.Errorf("doubling script (%d bytes): %d MiB allocated", len(s), n>>20)
	}
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("V" + strings.Repeat("x", 4) + strconv.Itoa(i) + "=1\n")
	}
	b.WriteString(strings.Repeat("true\n", 4000))
	b.WriteString("npm ci\n")
	if n := allocated(func() { Analyze(op(t, b.String(), nil, "", "/app"), nil) }); n > 128<<20 {
		t.Errorf("many variables, many commands (%d bytes): %d MiB allocated", b.Len(), n>>20)
	}
	// What a step really does still reads.
	got := summary(Analyze(op(t, "export D=/c && X=$D/npm && npm ci --cache $X", nil, "", "/app"), nil))
	if len(got) != 1 || !strings.Contains(got[0], "[/c/npm]") {
		t.Errorf("expansion: %q", got)
	}
	// A value past the bound stays unexpanded, so no path is made of it.
	long := strings.Repeat("y", maxValue)
	if v := expand("$A/$A", map[string]string{"A": long}); v != "$A/$A" {
		t.Errorf("long value expanded to %d bytes", len(v))
	}
}

// An editable install of the project ("-e .") loses the -e with the
// project folder in the early install: "-e -r file" is not a command.
func TestFixEditableOrder(t *testing.T) {
	for _, c := range []struct{ run, want string }{
		{"RUN pip install -e .", "&& pip install -r /tmp/requirements.txt\n"},
		{"RUN pip install --no-cache-dir --editable .", "&& pip install --no-cache-dir -r /tmp/requirements.txt\n"},
		{"RUN uv pip install --system -e .", "RUN uv pip install --system -r pyproject.toml\n"},
		{"RUN pip install -rrequirements.txt", "&& pip install -r/tmp/requirements-0.txt\n"},
	} {
		script := strings.TrimPrefix(c.run, "RUN ")
		s := Installs(op(t, script, []string{"PYTHON_VERSION=3.12.4"}, "", "/app"), c.run)
		if s == nil {
			t.Fatalf("%s: no installs", c.run)
		}
		f := s.OrderFragment("COPY . .", true)
		if !strings.Contains(f, c.want) || strings.Contains(f, "-e -r") || strings.Contains(f, "--editable -r") {
			t.Errorf("%s:\n%s\nwant a line with %q", c.run, f, c.want)
		}
	}
}

// A here-document's name is BuildKit's summary of it, not its script:
// no fragment is written from it.
func TestFixHeredocFragment(t *testing.T) {
	hs := Analyze(op(t, "apt-get update\napt-get install -y curl\n", nil, "", "/"), nil)
	if len(hs) == 0 {
		t.Fatal("no hint")
	}
	for _, name := range []string{"RUN <<EOF (apt-get update...)", "RUN --mount=type=secret,id=x <<-EOT (apt-get update...)"} {
		if f := Fragment(name, hs); f != "" {
			t.Errorf("%s: fragment\n%s", name, f)
		}
	}
}

// A file the named ones may pull in (an included requirements file, a
// gemspec, patches applied on install) is a reason to reinstall.
func TestFixMattersIncluded(t *testing.T) {
	for _, c := range []struct {
		script string
		file   string
		want   bool
	}{
		{"pip install -r requirements/prod.txt", "requirements/base.txt", true},
		{"pip install -r requirements/prod.txt", "requirements/prod.txt", true},
		{"pip install -r requirements.txt", "requirements-dev.txt", true},
		{"pip install -r deps/main.txt", "deps/extra.txt", true},
		{"pip install -r deps/main.txt", "docs/notes.txt", false},
		{"pip install -r requirements.txt", "src/app.py", false},
		{"pip install .", "requirements/base.in", true},
		{"uv pip install -r requirements/prod.txt", "requirements/base.txt", true},
		{"bundle install", "mygem.gemspec", true},
		{"bundle install", "lib/x.rb", false},
		{"npm ci", "patches/left-pad+1.0.0.patch", true},
		{"npm ci", "src/patches.ts", false},
		{"yarn install", ".yarn/patches/x.patch", true},
		{"yarn install", ".yarn/releases/yarn-4.5.0.cjs", true},
		{"pnpm install", "patches/a.patch", true},
		{"composer install", "patches/b.patch", true},
	} {
		s := Installs(op(t, c.script, nil, "", "/app"), "RUN "+c.script)
		if s == nil {
			t.Fatalf("%s: no installs", c.script)
		}
		if got := s.Matters(c.file); got != c.want {
			t.Errorf("%s: %s matters %v, want %v", c.script, c.file, got, c.want)
		}
	}
}

// Next's fragment gives no fixed id, which would make every project on
// the daemon share one cache, and the caveat says what an id does.
func TestFixNextNoFixedID(t *testing.T) {
	for _, cwd := range []string{"/app", "/srv/web"} {
		hs := Analyze(op(t, "npx next build", nil, "", cwd), nil)
		f := Fragment("RUN npx next build", hs)
		if f == "" || strings.Contains(f, ",id=") {
			t.Errorf("%s: fragment %q", cwd, f)
		}
		for _, h := range hs {
			for _, c := range h.Caveats {
				if strings.Contains(c, "as in the fragment") {
					t.Errorf("caveat: %s", c)
				}
			}
		}
	}
}

// The fragment leaves out what empties the cache it mounts, and dnf's
// keepcache goes to each of its commands.
func TestFixWipeDropped(t *testing.T) {
	for _, c := range []struct {
		name, cmd string
		mounts    []mnt
		earlier   string
		want      string // the fragment's last line
		caveat    string
	}{
		{name: "apt idiom", cmd: "apt-get update && apt-get install -y curl && apt-get clean && rm -rf /var/lib/apt/lists/*",
			want: "    apt-get update && apt-get install -y curl", caveat: "Take rm /var/lib/apt/lists/* out of the step"},
		{name: "apt idiom, mounted", cmd: "apt-get update && apt-get install -y curl && rm -rf /var/lib/apt/lists/*",
			mounts: []mnt{"/var/cache/apt:locked", "/var/lib/apt:locked"}, earlier: "rm -f /etc/apt/apt.conf.d/docker-clean",
			want: "    apt-get update && apt-get install -y curl"},
		{name: "rm of other folders too stays", cmd: "apt-get install -y curl && rm -rf /var/lib/apt/lists/* /tmp/build",
			want: "    apt-get install -y curl && rm -rf /var/lib/apt/lists/* /tmp/build"},
		{name: "wipe first", cmd: "apt-get clean && apt-get install -y curl",
			want: "    apt-get install -y curl"},
		{name: "dnf clean", cmd: "dnf install -y gcc && dnf clean all",
			want: "    dnf --setopt=keepcache=1 install -y gcc"},
		{name: "dnf twice", cmd: "dnf install -y gcc && dnf install -y make",
			want: "    dnf --setopt=keepcache=1 install -y gcc && dnf --setopt=keepcache=1 install -y make"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var anc []json.RawMessage
			if c.earlier != "" {
				anc = append(anc, op(t, c.earlier, nil, "", "/"))
			}
			hs := Analyze(op(t, c.cmd, nil, "", "/", c.mounts...), anc)
			name := "RUN "
			for _, m := range c.mounts {
				target, _, _ := strings.Cut(string(m), ":")
				name += "--mount=type=cache,target=" + target + ",sharing=locked "
			}
			f := Fragment(name+c.cmd, hs)
			lines := strings.Split(f, "\n")
			if last := lines[len(lines)-1]; last != c.want {
				t.Errorf("fragment:\n%s\nlast line %q, want %q", f, last, c.want)
			}
			if c.caveat != "" {
				found := false
				for _, h := range hs {
					for _, cv := range h.Caveats {
						found = found || strings.Contains(cv, c.caveat)
					}
				}
				if !found {
					t.Errorf("no caveat %q in %+v", c.caveat, hs)
				}
			}
		})
	}
}

// A flag that switches the manager's cache off is dropped from its own
// command only.
func TestFixDropScoped(t *testing.T) {
	cmd := "uv sync -n && head -n 5 x && echo -n done"
	f := Fragment("RUN "+cmd, Analyze(op(t, cmd, nil, "", "/app"), nil))
	if !strings.HasSuffix(f, "uv sync && head -n 5 x && echo -n done") {
		t.Errorf("fragment:\n%s", f)
	}
	// Not cut into commands: a one-letter flag stays where it is.
	cmd = "uv sync -n x; head -n 5 x"
	f = Fragment("RUN "+cmd, Analyze(op(t, cmd, nil, "", "/app"), nil))
	if !strings.HasSuffix(f, "uv sync -n x; head -n 5 x") {
		t.Errorf("fragment:\n%s", f)
	}
}

// sharing=private gives each build its own copy: nothing fails on the
// lock, so it is not "not locked".
func TestFixPrivateNotLocked(t *testing.T) {
	cmd := "rm -f /etc/apt/apt.conf.d/docker-clean && apt-get update && apt-get install -y curl"
	if got := summary(Analyze(op(t, cmd, nil, "", "/", "/var/cache/apt:private", "/var/lib/apt:private"), nil)); len(got) != 0 {
		t.Errorf("private: %q", got)
	}
	got := summary(Analyze(op(t, cmd, nil, "", "/", "/var/cache/apt", "/var/lib/apt"), nil))
	if len(got) != 1 || !strings.Contains(got[0], "not-locked@/var/cache/apt") {
		t.Errorf("shared: %q", got)
	}
}

// The order hint's texts come from the step's name, which the console
// masks, never from the operation, which holds the secrets in clear.
func TestFixOrderMasked(t *testing.T) {
	raw := "pip install --no-cache-dir --extra-index-url https://ci:glpat-ZZSECRET0123456789@pypi.example.com/simple . && curl -H 'Authorization: Bearer ZZBEARER' https://example.com/x"
	name := "RUN pip install --no-cache-dir --extra-index-url https://ci:[masked]@pypi.example.com/simple . && curl -H 'Authorization: Bearer [masked]' https://example.com/x"
	s := Installs(op(t, raw, []string{"PYTHON_VERSION=3.12.4"}, "", "/app"), name)
	if s == nil {
		t.Fatal("no installs")
	}
	texts := append([]string{s.OrderFragment("COPY . .", true)}, s.Others...)
	all := strings.Join(texts, "\n")
	if strings.Contains(all, "ZZSECRET") || strings.Contains(all, "ZZBEARER") {
		t.Errorf("secret in clear:\n%s", all)
	}
	if !strings.Contains(all, "[masked]") || len(s.Others) != 1 || texts[0] == "" {
		t.Errorf("want the masked texts, got others %q, fragment:\n%s", s.Others, texts[0])
	}
	// A name that does not show the operation's commands gives no text.
	s = Installs(op(t, raw, []string{"PYTHON_VERSION=3.12.4"}, "", "/app"), `RUN ["sh" "-c" "x"]`)
	if s == nil {
		t.Fatal("no installs")
	}
	if len(s.Others) != 0 || s.OrderFragment("COPY . .", true) != "" {
		t.Errorf("texts without a name for them: %q", s.Others)
	}
}
