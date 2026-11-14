package hints

import (
	"strings"
	"testing"
)

// orderCase is a RUN step right after a COPY of the whole context, from a
// real project of the 101-project lab run (2026-11-10), with the fragment
// the hint must give. Each fragment was built on BuildKit v0.34.2 (lab,
// 2026-11-14): cold, then after a one-line edit of a code file, where the
// install of the dependencies came from the cache and the image passed a
// smoke command.
type orderCase struct {
	name     string
	copy     string
	run      string // the RUN instruction as BuildKit shows it
	env      []string
	cwd      string
	managers []string
	only     bool
	fragment string
}

var orderCases = []orderCase{
	{
		name: "tradingagents (pip install .)",
		copy: "COPY . .", run: "RUN pip install --no-cache-dir .",
		env: []string{"PYTHON_VERSION=3.13.11"}, cwd: "/build", managers: []string{"pip"}, only: true,
		fragment: `COPY pyproject.toml setup.py* setup.cfg* /build/
RUN python -c "import pathlib, tomllib; f = pathlib.Path('pyproject.toml'); p = tomllib.loads(f.read_text()).get('project', {}) if f.exists() else {}; print('\n'.join(p.get('dependencies', [])))" > /tmp/requirements.txt \
 && pip install --no-cache-dir -r /tmp/requirements.txt
COPY . .
RUN pip install --no-cache-dir .`,
	},
	{
		name: "graphify (pip install with an extra)",
		copy: "COPY . /app", run: `RUN pip install --no-cache-dir ".[mcp]"`,
		env: []string{"PYTHON_VERSION=3.12.12"}, cwd: "/app", managers: []string{"pip"}, only: true,
		fragment: `COPY pyproject.toml setup.py* setup.cfg* /app/
RUN python -c "import pathlib, tomllib; f = pathlib.Path('pyproject.toml'); p = tomllib.loads(f.read_text()).get('project', {}) if f.exists() else {}; print('\n'.join(p.get('dependencies', []) + p.get('optional-dependencies', {}).get('mcp', [])))" > /tmp/requirements.txt \
 && pip install --no-cache-dir -r /tmp/requirements.txt
COPY . /app
RUN pip install --no-cache-dir ".[mcp]"`,
	},
	{
		name: "croc (npm ci, then a script that needs the code)",
		copy: "COPY . .", run: "RUN npm ci --prefix web     && npm run embed --prefix web",
		cwd: "/go/croc", managers: []string{"npm"},
		fragment: `COPY --parents web/**/package.json web/package-lock.json* web/npm-shrinkwrap.json* web/.npmrc* /go/croc/
RUN npm ci --prefix web
COPY . .
RUN npm run embed --prefix web`,
	},
	{
		name: "designpatternsphp (composer install)",
		copy: "ADD . /app", run: "RUN composer install",
		cwd: "/app", managers: []string{"composer"}, only: true,
		fragment: `COPY composer.json composer.lock* /app/
RUN composer install --no-autoloader --no-scripts
ADD . /app
RUN composer install`,
	},
}

func TestOrderFragments(t *testing.T) {
	for _, c := range orderCases {
		t.Run(c.name, func(t *testing.T) {
			script, _ := strings.CutPrefix(c.run, "RUN ")
			_, script = runFlags(script)
			s := Installs(op(t, script, c.env, "", c.cwd), c.run)
			if s == nil {
				t.Fatal("no installs")
			}
			if got := s.Managers(); strings.Join(got, ",") != strings.Join(c.managers, ",") {
				t.Errorf("managers %v, want %v", got, c.managers)
			}
			if s.Only != c.only {
				t.Errorf("only %v, want %v (others %v)", s.Only, c.only, s.Others)
			}
			if got := s.OrderFragment(c.copy, true); got != c.fragment {
				t.Errorf("fragment:\n%s\nwant:\n%s", got, c.fragment)
			}
		})
	}
}

// What is not an install the hint can judge, and what it does not move.
func TestOrderInstalls(t *testing.T) {
	installs := map[string]string{ // script -> managers ("" for none)
		"pip install flask gunicorn":                           "pip",
		"pip install --no-cache-dir -r requirements.txt":       "pip",
		"pip install -r requirements.txt /app/packages/x[all]": "", // a folder of the context: its code
		"pip install ./libs/core":                              "",
		"pip install dist/app-1.0-py3-none-any.whl":            "",
		"pip install --target /opt/deps -r requirements.txt":   "pip", // a valued option is not a path
		"pip download -r requirements.txt":                     "",
		"uv sync --frozen":                                     "uv",
		"uv pip install -e ./pkg":                              "",
		"npm ci":                                               "npm",
		"npm install":                                          "npm",
		"npm install -g pnpm":                                  "",
		"npm install left-pad":                                 "",
		"npm run build":                                        "",
		"yarn":                                                 "yarn",
		"yarn --network-timeout 600000 install":                "yarn",
		"yarn build":                                           "",
		"yarn --cwd web build":                                 "",
		"pnpm install --frozen-lockfile":                       "pnpm",
		"pnpm add zod":                                         "",
		"go mod download":                                      "go",
		"go build ./...":                                       "",
		"cargo build --release":                                "",
		"cargo fetch":                                          "cargo",
		"bundle install":                                       "bundler",
		"bundle exec rake assets:precompile":                   "",
		"composer install --no-dev":                            "composer",
		"composer dump-autoload":                               "",
		"mix deps.get":                                         "mix",
		"apt-get install -y curl":                              "",
		"poetry install --only main":                           "poetry",
		"poetry build":                                         "",
	}
	for script, want := range installs {
		s := Installs(op(t, script, nil, "", "/app"), "RUN "+script)
		got := ""
		if s != nil {
			got = strings.Join(s.Managers(), ",")
		}
		if got != want {
			t.Errorf("%q: managers %q, want %q", script, got, want)
		}
	}

	frag := func(copy, run, cwd string, parents bool, env ...string) string {
		script, _ := strings.CutPrefix(run, "RUN ")
		_, script = runFlags(script)
		s := Installs(op(t, script, env, "", cwd), run)
		if s == nil {
			t.Fatalf("%q: no installs", run)
		}
		return s.OrderFragment(copy, parents)
	}
	none := []struct{ copy, run, cwd string }{
		{"COPY . .", "RUN poetry config virtualenvs.create false && poetry install", "/app"}, // set up first
		{"COPY . .", "RUN npm ci; npm run build", "/app"},                                    // not "a && b"
		{"COPY . .", "RUN cd web && npm ci", "/app"},                                         // cd: not moved
		{"COPY --from=deps . .", "RUN npm ci", "/app"},                                       // not the context
		{"COPY src/ .", "RUN npm ci", "/app"},                                                // part of it
		{"COPY . /app", "RUN npm ci --prefix /opt/ui", "/app"},                               // not inside the copy
		{"COPY . .", "RUN cargo fetch", "/app"},                                              // no way alone
		{"COPY . .", "RUN bundle install", "/app"},                                           // gemspec
		{"COPY . .", "RUN pip install .", "/app"},                                            // no tomllib known
	}
	for _, c := range none {
		if f := frag(c.copy, c.run, c.cwd, true); f != "" {
			t.Errorf("%s / %s: fragment\n%s", c.copy, c.run, f)
		}
	}
	if f := frag("COPY . .", "RUN pip install .", "/app", true, "PYTHON_VERSION=3.10.14"); f != "" {
		t.Errorf("Python 3.10 has no tomllib: %s", f)
	}
	// A workspace needs every package.json: without COPY --parents, none.
	if f := frag("COPY . .", "RUN npm ci", "/app", false); f != "" {
		t.Errorf("without --parents: %s", f)
	}
	for _, c := range []struct{ copy, run, cwd, want string }{
		{"COPY --chown=app:app . /srv/app", "RUN npm ci && npm run build", "/srv/app",
			"COPY --parents --chown=app:app **/package.json package-lock.json* npm-shrinkwrap.json* .npmrc* /srv/app/\nRUN npm ci\nCOPY --chown=app:app . /srv/app\nRUN npm run build"},
		{"COPY . .", "RUN go mod download", "/src",
			"COPY go.mod go.sum* /src/\nRUN go mod download\nCOPY . ."},
		{"COPY . .", "RUN --mount=type=cache,target=/root/.cache/pip pip install flask", "/app",
			"RUN --mount=type=cache,target=/root/.cache/pip pip install flask\nCOPY . ."},
		{"COPY . .", "RUN uv sync --locked", "/app",
			"COPY pyproject.toml uv.lock* /app/\nRUN uv sync --no-install-project --locked\nCOPY . .\nRUN uv sync --locked"},
		{"COPY . .", "RUN pip install -r requirements.txt", "/app",
			"COPY requirements.txt /app/\nRUN sed -E '/^[[:space:]]*(-e|--editable|-r|--requirement|-c|--constraint|[.]|[/]|file:)/d' requirements.txt > /tmp/requirements-0.txt \\\n && pip install -r /tmp/requirements-0.txt\nCOPY . .\nRUN pip install -r requirements.txt"},
	} {
		if f := frag(c.copy, c.run, c.cwd, true); f != c.want {
			t.Errorf("%s / %s:\n%s\nwant:\n%s", c.copy, c.run, f, c.want)
		}
	}
	s := Installs(op(t, "npm ci", nil, "", "/app"), "RUN npm ci")
	for file, want := range map[string]bool{"package.json": true, "packages/ui/package.json": true, "package-lock.json": true,
		".npmrc": true, "src/index.ts": false, "README.md": false, "pyproject.toml": false} {
		if s.Matters(file) != want {
			t.Errorf("npm: %s matters %v", file, !want)
		}
	}
}
