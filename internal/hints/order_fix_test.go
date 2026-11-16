package hints

import (
	"strings"
	"testing"
)

// What the order hint must not count as an install of the dependencies.
func TestOrderNotInstalls(t *testing.T) {
	for script, want := range map[string]string{ // script -> managers ("" for none)
		"pnpm install zod":           "", // a named package: no file of the project
		"pnpm i zod":                 "",
		"pnpm install":               "pnpm",
		"poetry install --only-root": "", // the project alone: it needs the code
		"poetry install --no-root":   "poetry",
	} {
		s := Installs(op(t, script, nil, "", "/app"), "RUN "+script)
		got := ""
		if s != nil {
			got = strings.Join(s.Managers(), ",")
		}
		if got != want {
			t.Errorf("%q: managers %q, want %q", script, got, want)
		}
	}
}

// Scripts and files a fragment must not move or copy, and the Python
// version from which pyproject.toml can be read.
func TestOrderFragmentEdges(t *testing.T) {
	frag := func(copy, run, cwd string, env ...string) string {
		t.Helper()
		script, _ := strings.CutPrefix(run, "RUN ")
		_, script = runFlags(script)
		s := Installs(op(t, script, env, "", cwd), run)
		if s == nil {
			t.Fatalf("%q: no installs", run)
		}
		return s.OrderFragment(copy, true)
	}
	for _, c := range []struct{ run, why string }{
		{"RUN npm ci | tee log", "a pipe"},
		{"RUN npm ci && npm run build || echo failed", "|| runs the last command when the install fails"},
		{"RUN npm ci # install && npm run build", "the comment hides npm run build from the shell"},
		{"RUN pip install -r ../requirements.txt", "a file outside the copied folder"},
	} {
		if f := frag("COPY . .", c.run, "/app"); f != "" {
			t.Errorf("%s (%s): fragment\n%s", c.run, c.why, f)
		}
	}

	want := `COPY pyproject.toml setup.py* setup.cfg* /app/
RUN python -c "import pathlib, tomllib; f = pathlib.Path('pyproject.toml'); p = tomllib.loads(f.read_text()).get('project', {}) if f.exists() else {}; print('\n'.join(p.get('dependencies', [])))" > /tmp/requirements.txt \
 && pip install -r /tmp/requirements.txt
COPY . .
RUN pip install .`
	for v, w := range map[string]string{"3.11": want, "3.11.0": want, "3.10.14": "", "3.10": ""} {
		if f := frag("COPY . .", "RUN pip install .", "/app", "PYTHON_VERSION="+v); f != w {
			t.Errorf("Python %s: fragment\n%s\nwant:\n%s", v, f, w)
		}
	}
}
