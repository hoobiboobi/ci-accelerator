package hints

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mnt is a cache mount in a test operation: "target" or
// "target:locked".
type mnt string

// op builds a step's operation as BuildKit records it.
func op(t testing.TB, cmd string, env []string, user, cwd string, mounts ...mnt) json.RawMessage {
	t.Helper()
	ms := []map[string]any{{"dest": "/"}}
	for _, m := range mounts {
		dest, opt, _ := strings.Cut(string(m), ":")
		x := map[string]any{"dest": dest, "mountType": 3, "input": -1, "output": -1, "cacheOpt": map[string]any{"ID": dest}}
		if opt == "locked" {
			x["cacheOpt"].(map[string]any)["sharing"] = 2
		}
		if opt == "private" {
			x["cacheOpt"].(map[string]any)["sharing"] = 1
		}
		if opt == "owned" {
			// uid, gid or mode: the frontend makes the mount from a folder
			// it creates (an input of the step).
			x["input"] = 1
		}
		ms = append(ms, x)
	}
	meta := map[string]any{"args": []string{"/bin/sh", "-c", cmd}, "env": append([]string{"PATH=/usr/bin:/bin"}, env...), "cwd": cwd}
	if user != "" {
		meta["user"] = user
	}
	b, err := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": meta, "mounts": ms}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// summary is a hint in one line: manager, problems, caches.
func summary(hs []Hint) []string {
	var out []string
	for _, h := range hs {
		var ps []string
		for _, p := range h.Problems {
			s := string(p.Kind)
			if p.Mount != "" {
				s += "@" + p.Mount
			}
			if p.Setting != "" {
				s += "(" + p.Setting + ")"
			}
			ps = append(ps, s)
		}
		var cs []string
		for _, c := range h.Caches {
			cs = append(cs, c.Path)
		}
		out = append(out, fmt.Sprintf("%s %s [%s]", h.Manager, strings.Join(ps, " "), strings.Join(cs, " ")))
	}
	return out
}

func TestAnalyze(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		env    []string
		user   string
		cwd    string
		mounts []mnt
		want   []string
	}{
		// npm
		{name: "npm ci without a mount", cmd: "npm ci", cwd: "/app", want: []string{"npm missing [/root/.npm]"}},
		{name: "npm with its cache mounted", cmd: "npm ci", cwd: "/app", mounts: []mnt{"/root/.npm"}},
		{name: "npm cache under a parent mount", cmd: "npm ci", cwd: "/app", mounts: []mnt{"/root"}},
		{name: "npm run build is not an install", cmd: "npm run build", cwd: "/app"},
		{name: "npm as another user without HOME", cmd: "npm ci", user: "node", cwd: "/app", want: []string{"npm missing []"}},
		{name: "npm as another user with HOME", cmd: "npm ci", user: "node", env: []string{"HOME=/home/node"}, cwd: "/app", want: []string{"npm missing [/home/node/.npm]"}},
		{name: "npm mount for root while running as node", cmd: "npm ci", user: "node", env: []string{"HOME=/home/node"}, cwd: "/app", mounts: []mnt{"/root/.npm"},
			want: []string{"npm wrong-path@/root/.npm [/home/node/.npm]"}},
		{name: "npm cache moved by a variable", cmd: "npm ci", env: []string{"npm_config_cache=/tmp/npm"}, cwd: "/app", mounts: []mnt{"/root/.npm"},
			want: []string{"npm wrong-path@/root/.npm [/tmp/npm]"}},
		{name: "npm cache moved by a flag", cmd: "npm ci --cache /c", cwd: "/app", mounts: []mnt{"/c"}},
		{name: "npm unknown user, mount looks right but has no owner", cmd: "npm ci", user: "1000", cwd: "/app", mounts: []mnt{"/home/x/.npm"},
			want: []string{"npm not-writable@/home/x/.npm []"}},
		{name: "npm unknown user, mount looks right and has an owner", cmd: "npm ci", user: "1000", cwd: "/app", mounts: []mnt{"/home/x/.npm:owned"}},
		{name: "npm as node with root's cache mounted", cmd: "npm ci", user: "node", cwd: "/app", mounts: []mnt{"/root/.npm"},
			want: []string{"npm wrong-path@/root/.npm []"}},
		{name: "npm as uid 1000, mount without owner", cmd: "npm ci", user: "1000", env: []string{"HOME=/home/app"}, cwd: "/app", mounts: []mnt{"/home/app/.npm"},
			want: []string{"npm not-writable@/home/app/.npm [/home/app/.npm]"}},
		{name: "micromamba as its user, mount without owner", cmd: "micromamba install -y zlib", user: "mambauser", env: []string{"MAMBA_ROOT_PREFIX=/opt/conda"}, cwd: "/", mounts: []mnt{"/opt/conda/pkgs"},
			want: []string{"conda not-writable@/opt/conda/pkgs [/opt/conda/pkgs]"}},
		{name: "mount with an owner", cmd: "micromamba install -y zlib", user: "mambauser", env: []string{"MAMBA_ROOT_PREFIX=/opt/conda"}, cwd: "/", mounts: []mnt{"/opt/conda/pkgs:owned"}},
		{name: "node_modules mounted", cmd: "npm ci", cwd: "/app", mounts: []mnt{"/root/.npm", "/app/node_modules"},
			want: []string{"npm image-needs@/app/node_modules(installs into /app/node_modules) [/root/.npm]"}},
		{name: "node_modules/.cache is fine", cmd: "npm ci", cwd: "/app", mounts: []mnt{"/root/.npm", "/app/node_modules/.cache"}},
		{name: "npm cache clean with a mount", cmd: "npm ci && npm cache clean --force", cwd: "/app", mounts: []mnt{"/root/.npm"},
			want: []string{"npm disabled(npm cache clean) [/root/.npm]"}},
		{name: "npx pnpm", cmd: "npx pnpm@9 install --frozen-lockfile", cwd: "/app", want: []string{"pnpm missing [/root/.local/share/pnpm/store]"}},
		{name: "corepack yarn", cmd: "corepack enable && corepack yarn install", cwd: "/app", want: []string{"yarn missing []"}},
		// shell reading
		{name: "after && in a subshell", cmd: "cd /src && (apk update; npm ci)", cwd: "/", want: []string{"npm missing [/root/.npm]"}},
		{name: "quoted && is not a separator", cmd: `echo "npm ci && pip install x"`, cwd: "/"},
		{name: "comment", cmd: "true # npm ci", cwd: "/"},
		{name: "sh -c nested", cmd: `sh -c "npm ci"`, cwd: "/app", want: []string{"npm missing [/root/.npm]"}},
		{name: "sudo env wrapper", cmd: "sudo -E env FOO=1 npm ci", cwd: "/app", want: []string{"npm missing [/root/.npm]"}},
		{name: "export moves the cache", cmd: "export npm_config_cache=/c && npm ci", cwd: "/app", mounts: []mnt{"/c"}},
		{name: "inline assignment", cmd: "PIP_NO_CACHE_DIR=1 pip install x", cwd: "/",
			want: []string{"pip missing [/root/.cache/pip]"}},
		// pip
		{name: "pip with --no-cache-dir and no mount", cmd: "pip install --no-cache-dir -r req.txt", cwd: "/", want: []string{"pip missing [/root/.cache/pip]"}},
		{name: "pip with --no-cache-dir and a mount", cmd: "pip install --no-cache-dir -r req.txt", cwd: "/", mounts: []mnt{"/root/.cache/pip"},
			want: []string{"pip disabled(--no-cache-dir) [/root/.cache/pip]"}},
		{name: "PIP_NO_CACHE_DIR from the image switches the mount off", cmd: "pip install x", env: []string{"PIP_NO_CACHE_DIR=1"}, cwd: "/", mounts: []mnt{"/root/.cache/pip"},
			want: []string{"pip disabled(PIP_NO_CACHE_DIR) [/root/.cache/pip]"}},
		{name: "UV_NO_CACHE switches the mount off", cmd: "uv sync", env: []string{"UV_NO_CACHE=1", "UV_LINK_MODE=copy"}, cwd: "/app", mounts: []mnt{"/root/.cache/uv"},
			want: []string{"uv disabled(UV_NO_CACHE) [/root/.cache/uv]"}},
		{name: "PIP_NO_CACHE_DIR=0 does not switch it off", cmd: "pip install x", env: []string{"PIP_NO_CACHE_DIR=0"}, cwd: "/", mounts: []mnt{"/root/.cache/pip"}},
		{name: "python -m pip", cmd: "python3 -m pip install x", cwd: "/", want: []string{"pip missing [/root/.cache/pip]"}},
		{name: "pip3.12", cmd: "pip3.12 install x", cwd: "/", want: []string{"pip missing [/root/.cache/pip]"}},
		{name: "pip --target in a mount", cmd: "pip install --target /deps x", cwd: "/", mounts: []mnt{"/root/.cache/pip", "/deps"},
			want: []string{"pip image-needs@/deps(installs into /deps) [/root/.cache/pip]"}},
		{name: "XDG_CACHE_HOME", cmd: "pip install x", env: []string{"XDG_CACHE_HOME=/xdg"}, cwd: "/", want: []string{"pip missing [/xdg/pip]"}},
		{name: "pip list is not an install", cmd: "pip list", cwd: "/"},
		// uv / poetry
		{name: "uv sync", cmd: "uv sync --locked", cwd: "/app", want: []string{"uv missing [/root/.cache/uv]"}},
		{name: "uv --no-cache with mount", cmd: "uv sync --no-cache", cwd: "/app", mounts: []mnt{"/root/.cache/uv"}, env: []string{"UV_LINK_MODE=copy"},
			want: []string{"uv disabled(--no-cache) [/root/.cache/uv]"}},
		{name: "poetry whole cache mounted", cmd: "poetry install", cwd: "/app", mounts: []mnt{"/root/.cache/pypoetry"},
			want: []string{"poetry image-needs@/root/.cache/pypoetry(installs into /root/.cache/pypoetry/virtualenvs) [/root/.cache/pypoetry/cache /root/.cache/pypoetry/artifacts]"}},
		{name: "poetry whole cache, no venvs", cmd: "poetry install", env: []string{"POETRY_VIRTUALENVS_CREATE=false"}, cwd: "/app", mounts: []mnt{"/root/.cache/pypoetry"}},
		// apt
		{name: "apt without a mount", cmd: "apt-get update && apt-get install -y curl", cwd: "/",
			want: []string{"apt missing missing [/var/cache/apt /var/lib/apt]"}},
		{name: "apt mounted, shared, docker-clean kept", cmd: "apt-get update && apt-get install -y curl", cwd: "/", mounts: []mnt{"/var/cache/apt", "/var/lib/apt"},
			want: []string{"apt disabled(docker-clean) not-locked@/var/cache/apt not-locked@/var/lib/apt [/var/cache/apt /var/lib/apt]"}},
		{name: "apt done right", cmd: "rm -f /etc/apt/apt.conf.d/docker-clean && apt-get update && apt-get install -y curl", cwd: "/", mounts: []mnt{"/var/cache/apt:locked", "/var/lib/apt:locked"}},
		{name: "apt clean empties the mount", cmd: "rm -f /etc/apt/apt.conf.d/docker-clean; apt-get update && apt-get install -y curl && apt-get clean", cwd: "/", mounts: []mnt{"/var/cache/apt:locked", "/var/lib/apt:locked"},
			want: []string{"apt disabled(apt-get clean) [/var/cache/apt /var/lib/apt]"}},
		{name: "apt binary needs Keep-Downloaded-Packages", cmd: "rm -f /etc/apt/apt.conf.d/docker-clean; apt update && apt install -y curl", cwd: "/", mounts: []mnt{"/var/cache/apt:locked", "/var/lib/apt:locked"},
			want: []string{"apt disabled(docker-clean) [/var/cache/apt /var/lib/apt]"}},
		{name: "apt-get update alone", cmd: "apt-get update", cwd: "/"},
		// apk, dnf, zypper
		{name: "apk --no-cache, no mount", cmd: "apk add --no-cache git", cwd: "/", want: []string{"apk missing [/etc/apk/cache]"}},
		{name: "apk mounted shared", cmd: "apk add git", cwd: "/", mounts: []mnt{"/etc/apk/cache"}, want: []string{"apk not-locked@/etc/apk/cache [/etc/apk/cache]"}},
		{name: "apk mounted locked with --no-cache", cmd: "apk add --no-cache git", cwd: "/", mounts: []mnt{"/etc/apk/cache:locked"},
			want: []string{"apk disabled(--no-cache) [/etc/apk/cache]"}},
		{name: "dnf without keepcache", cmd: "dnf install -y git", cwd: "/", mounts: []mnt{"/var/cache/dnf:locked", "/var/cache/libdnf5:locked"},
			want: []string{"dnf disabled(keepcache) [/var/cache/dnf /var/cache/libdnf5]"}},
		{name: "dnf with keepcache", cmd: "dnf install -y --setopt=keepcache=1 git", cwd: "/", mounts: []mnt{"/var/cache/dnf:locked", "/var/cache/libdnf5:locked"}},
		{name: "yum", cmd: "yum install -y git", cwd: "/", want: []string{"dnf missing [/var/cache/yum]"}},
		{name: "microdnf", cmd: "microdnf install -y git", cwd: "/", want: []string{"dnf missing [/var/cache/yum]"}},
		{name: "zypper", cmd: "zypper --non-interactive install git", cwd: "/", want: []string{"zypper missing [/var/cache/zypp]"}},
		// go
		{name: "go build in the golang image", cmd: "go build -o /app ./cmd/app", env: []string{"GOPATH=/go"}, cwd: "/src",
			want: []string{"go missing missing [/go/pkg/mod /root/.cache/go-build]"}},
		{name: "go mod download needs only the module cache", cmd: "go mod download", env: []string{"GOPATH=/go"}, cwd: "/src", mounts: []mnt{"/go/pkg/mod"}},
		{name: "go with both", cmd: "go build ./...", env: []string{"GOPATH=/go"}, cwd: "/src", mounts: []mnt{"/go/pkg/mod", "/root/.cache/go-build"}},
		{name: "go vendored", cmd: "go build -mod=vendor ./...", env: []string{"GOPATH=/go"}, cwd: "/src", mounts: []mnt{"/root/.cache/go-build"}},
		{name: "go clean -modcache", cmd: "go mod download && go clean -modcache", env: []string{"GOPATH=/go"}, cwd: "/src", mounts: []mnt{"/go/pkg/mod"},
			want: []string{"go disabled(go clean -modcache) [/go/pkg/mod]"}},
		{name: "go install into a mounted GOPATH", cmd: "go install ./cmd/x", env: []string{"GOPATH=/go"}, cwd: "/src", mounts: []mnt{"/go", "/root/.cache/go-build"},
			want: []string{"go image-needs@/go(installs into /go/bin) [/go/pkg/mod /root/.cache/go-build]"}},
		// cargo
		{name: "cargo in the rust image", cmd: "cargo build --release", env: []string{"CARGO_HOME=/usr/local/cargo"}, cwd: "/app",
			want: []string{"cargo missing missing [/usr/local/cargo/registry /usr/local/cargo/git]"}},
		{name: "cargo target mounted, copied out", cmd: "cargo build --release && cp target/release/app /bin/app", env: []string{"CARGO_HOME=/usr/local/cargo"}, cwd: "/app",
			mounts: []mnt{"/usr/local/cargo/registry", "/usr/local/cargo/git", "/app/target"}},
		{name: "cargo target mounted, not copied out", cmd: "cargo build --release", env: []string{"CARGO_HOME=/usr/local/cargo"}, cwd: "/app",
			mounts: []mnt{"/usr/local/cargo/registry", "/usr/local/cargo/git", "/app/target"},
			want:   []string{"cargo image-needs@/app/target(build output in /app/target is not copied out) [/usr/local/cargo/registry /usr/local/cargo/git]"}},
		// JVM, .NET, PHP, Ruby, others
		{name: "maven wrapper", cmd: "./mvnw -B package -DskipTests", cwd: "/app", want: []string{"maven missing [/root/.m2/repository]"}},
		{name: "maven repo flag", cmd: "mvn -Dmaven.repo.local=/m2 package", cwd: "/app", mounts: []mnt{"/m2"}},
		{name: "gradle wrapper", cmd: "./gradlew build --no-daemon", cwd: "/app", want: []string{"gradle missing missing [/root/.gradle/caches /root/.gradle/wrapper]"}},
		{name: "gradle user home", cmd: "gradle build", env: []string{"GRADLE_USER_HOME=/g"}, cwd: "/app", mounts: []mnt{"/g"}},
		{name: "dotnet restore", cmd: "dotnet restore", cwd: "/src", want: []string{"nuget missing [/root/.nuget/packages]"}},
		{name: "composer in the composer image", cmd: "composer install --no-dev", env: []string{"COMPOSER_HOME=/tmp"}, cwd: "/app", want: []string{"composer missing [/tmp/cache]"}},
		{name: "composer without COMPOSER_HOME", cmd: "composer install", cwd: "/app", want: []string{"composer missing []"}},
		{name: "bundle install in the ruby image", cmd: "bundle install", env: []string{"GEM_HOME=/usr/local/bundle"}, cwd: "/app",
			want: []string{"bundler missing [/usr/local/bundle/cache]"}},
		{name: "gem home mounted", cmd: "bundle install", env: []string{"GEM_HOME=/usr/local/bundle"}, cwd: "/app", mounts: []mnt{"/usr/local/bundle"},
			want: []string{"bundler image-needs@/usr/local/bundle(installs into /usr/local/bundle) [/usr/local/bundle/cache]"}},
		{name: "dart pub get", cmd: "dart pub get", cwd: "/app", want: []string{"pub missing [/root/.pub-cache]"}},
		{name: "mix deps.get", cmd: "mix deps.get --only prod", cwd: "/app", want: []string{"hex missing [/root/.hex/packages]"}},
		{name: "deno image", cmd: "deno cache main.ts", env: []string{"DENO_DIR=/deno-dir/"}, cwd: "/app", want: []string{"deno missing [/deno-dir]"}},
		{name: "next build", cmd: "npx next build", cwd: "/app", want: []string{"next missing [/app/.next/cache]"}},
		{name: "whole .next mounted", cmd: "next build", cwd: "/app", mounts: []mnt{"/app/.next"},
			want: []string{"next image-needs@/app/.next(installs into /app/.next) [/app/.next/cache]"}},
		{name: "ccache only with a compiler", cmd: "make -j8", env: []string{"CC=ccache gcc", "CCACHE_DIR=/ccache"}, cwd: "/src", want: []string{"ccache missing [/ccache]"}},
		{name: "ccache env without a build", cmd: "echo hi", env: []string{"CC=ccache gcc"}, cwd: "/src"},
		{name: "two managers in one step", cmd: "apk add --no-cache python3 && pip install x", cwd: "/",
			want: []string{"apk missing [/etc/apk/cache]", "pip missing [/root/.cache/pip]"}},
		{name: "not a manager", cmd: "make build && ./run-tests", cwd: "/"},
		{name: "copying into the target mount is not copying out", cmd: "cargo build --release && cp README /app/target/", env: []string{"CARGO_HOME=/usr/local/cargo"}, cwd: "/app",
			mounts: []mnt{"/usr/local/cargo/registry", "/usr/local/cargo/git", "/app/target"},
			want:   []string{"cargo image-needs@/app/target(build output in /app/target is not copied out) [/usr/local/cargo/registry /usr/local/cargo/git]"}},
		// Shapes found in public Dockerfiles (2026-11-03), reduced: mounts
		// copied from documentation without the setting that goes with them.
		{name: "pnpm store mount without PNPM_HOME", cmd: "pnpm install --frozen-lockfile", cwd: "/app", mounts: []mnt{"/pnpm/store"},
			want: []string{"pnpm wrong-path@/pnpm/store [/root/.local/share/pnpm/store]"}},
		{name: "pnpm store mount with PNPM_HOME", cmd: "pnpm install --frozen-lockfile", env: []string{"PNPM_HOME=/pnpm"}, cwd: "/app", mounts: []mnt{"/pnpm/store"}},
		{name: "Gradle image as root, mount on the gradle user's home", cmd: "gradle clean build -x test --no-daemon", cwd: "/app", mounts: []mnt{"/home/gradle/.gradle"},
			want: []string{"gradle wrong-path@/home/gradle/.gradle [/root/.gradle/caches]"}},
		{name: "apk mount on /var/cache/apk with --no-cache", cmd: "apk add --no-cache ffmpeg", cwd: "/", mounts: []mnt{"/var/cache/apk"},
			want: []string{"apk wrong-path@/var/cache/apk [/etc/apk/cache]"}},
		{name: "apk mount on /var/cache/apk, linked from /etc/apk/cache", cmd: "ln -s /var/cache/apk /etc/apk/cache && apk add ffmpeg", cwd: "/", mounts: []mnt{"/var/cache/apk:locked"}},
		{name: "apt mount only on archives, apt binary, docker-clean kept", cmd: "apt update && apt install -y wget", cwd: "/", mounts: []mnt{"/var/cache/apt"},
			want: []string{"apt disabled(docker-clean) missing not-locked@/var/cache/apt [/var/cache/apt /var/lib/apt]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summary(Analyze(op(t, c.cmd, c.env, c.user, c.cwd, c.mounts...), nil))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestAncestors(t *testing.T) {
	// docker-clean removed by an earlier step counts.
	earlier := op(t, "rm -f /etc/apt/apt.conf.d/docker-clean", nil, "", "/")
	got := summary(Analyze(op(t, "apt-get update && apt-get install -y curl", nil, "", "/", "/var/cache/apt:locked", "/var/lib/apt:locked"), []json.RawMessage{earlier}))
	if len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
	// dnf keepcache from the configuration written earlier.
	earlier = op(t, "echo keepcache=True >> /etc/dnf/dnf.conf", nil, "", "/")
	got = summary(Analyze(op(t, "dnf install -y git", nil, "", "/", "/var/cache/dnf:locked", "/var/cache/libdnf5:locked"), []json.RawMessage{earlier}))
	if len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

func TestNotACommand(t *testing.T) {
	for _, raw := range []string{`{}`, `not json`, `{"Op":{"source":{"identifier":"local://context"}}}`, `{"Op":{"exec":{"meta":{"args":[]}}}}`} {
		if hs := Analyze(json.RawMessage(raw), nil); hs != nil {
			t.Errorf("%s: got %v", raw, hs)
		}
	}
}

func TestExecForm(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"npm", "ci"}, "cwd": "/app"}}}})
	if got := summary(Analyze(b, nil)); !reflect.DeepEqual(got, []string{"npm missing [/root/.npm]"}) {
		t.Errorf("got %q", got)
	}
	// SHELL ["/usr/local/bin/_dockerfile_shell.sh"] (micromamba's image)
	b, _ = json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"/usr/local/bin/_dockerfile_shell.sh", "micromamba install -y zlib"}, "env": []string{"MAMBA_ROOT_PREFIX=/opt/conda"}, "cwd": "/"}}}})
	if got := summary(Analyze(b, nil)); !reflect.DeepEqual(got, []string{"conda missing [/opt/conda/pkgs]"}) {
		t.Errorf("got %q", got)
	}
	// SHELL ["/bin/bash", "-o", "pipefail", "-c"]
	b, _ = json.Marshal(map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"/bin/bash", "-o", "pipefail", "-c", "npm ci | tee log"}, "cwd": "/app"}}}})
	if got := summary(Analyze(b, nil)); !reflect.DeepEqual(got, []string{"npm missing [/root/.npm]"}) {
		t.Errorf("got %q", got)
	}
}

func TestFragment(t *testing.T) {
	cases := []struct {
		name, step, cmd string
		user            string
		env             []string
		mounts          []mnt
		earlier         string
		want            string
	}{
		{name: "npm", step: "RUN npm ci", cmd: "npm ci",
			want: "RUN --mount=type=cache,target=/root/.npm npm ci"},
		{name: "pip drops --no-cache-dir", step: "RUN pip install --no-cache-dir -r requirements.txt", cmd: "pip install --no-cache-dir -r requirements.txt",
			want: "RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt"},
		{name: "apt", step: "RUN apt-get update && apt-get install -y curl", cmd: "apt-get update && apt-get install -y curl",
			want: "RUN rm -f /etc/apt/apt.conf.d/docker-clean; echo 'Binary::apt::APT::Keep-Downloaded-Packages \"true\";' > /etc/apt/apt.conf.d/keep-cache\n" +
				"RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \\\n    --mount=type=cache,target=/var/lib/apt,sharing=locked \\\n    apt-get update && apt-get install -y curl"},
		{name: "apt mounts made locked, docker-clean gone earlier", step: "RUN --mount=type=cache,target=/var/cache/apt --mount=type=cache,target=/var/lib/apt,sharing=shared apt-get install -y curl",
			cmd: "apt-get install -y curl", mounts: []mnt{"/var/cache/apt", "/var/lib/apt"}, earlier: "rm -f /etc/apt/apt.conf.d/docker-clean",
			want: "RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \\\n    --mount=type=cache,target=/var/lib/apt,sharing=locked \\\n    apt-get install -y curl"},
		{name: "secret mount kept, wrong mount replaced", step: "RUN --mount=type=secret,id=npmrc,target=/root/.npmrc --mount=type=cache,target=/root/.npm npm ci",
			cmd: "npm ci", env: []string{"HOME=/home/node"}, mounts: []mnt{"/root/.npm"},
			want: "RUN --mount=type=secret,id=npmrc,target=/root/.npmrc \\\n    --mount=type=cache,target=/home/node/.npm \\\n    npm ci"},
		{name: "dnf keepcache inserted", step: "RUN dnf install -y git", cmd: "dnf install -y git",
			want: "RUN --mount=type=cache,target=/var/cache/dnf,sharing=locked \\\n    --mount=type=cache,target=/var/cache/libdnf5,sharing=locked \\\n    dnf --setopt=keepcache=1 install -y git"},
		{name: "unknown path gets a placeholder", step: "RUN composer install", cmd: "composer install",
			want: "RUN --mount=type=cache,target=<composer cache folder> composer install"},
		{name: "go keeps the right mount", step: "RUN --mount=type=cache,target=/go/pkg/mod go build ./...", cmd: "go build ./...", env: []string{"GOPATH=/go"}, mounts: []mnt{"/go/pkg/mod"},
			want: "RUN --mount=type=cache,target=/go/pkg/mod \\\n    --mount=type=cache,target=/root/.cache/go-build \\\n    go build ./..."},
		{name: "next gets an id", step: "RUN next build", cmd: "next build",
			want: "RUN --mount=type=cache,target=/app/.next/cache,id=next-cache next build"},
		{name: "uv sets the link mode", step: "RUN uv sync", cmd: "uv sync",
			want: "ENV UV_LINK_MODE=copy\nRUN --mount=type=cache,target=/root/.cache/uv uv sync"},
		{name: "micromamba user gets a mode", step: "RUN micromamba install -y zlib", cmd: "micromamba install -y zlib", user: "mambauser", env: []string{"MAMBA_ROOT_PREFIX=/opt/conda"},
			want: "RUN --mount=type=cache,target=/opt/conda/pkgs,mode=0777 \\\n    micromamba install -y zlib"},
		{name: "numeric user gets its uid on the existing mount", step: "RUN --mount=type=cache,target=/c npm ci", cmd: "npm ci", user: "1000:1000", env: []string{"HOME=/h"}, mounts: []mnt{"/c"},
			want: "RUN --mount=type=cache,target=/c \\\n    --mount=type=cache,target=/h/.npm,uid=1000,gid=1000 \\\n    npm ci"},
		{name: "numeric user, its npm mount without owner", step: "RUN --mount=type=cache,target=/h/.npm npm ci", cmd: "npm ci", user: "1000", env: []string{"HOME=/h"}, mounts: []mnt{"/h/.npm"},
			want: "RUN --mount=type=cache,target=/h/.npm,uid=1000 npm ci"},
		{name: "a mount written with a trailing slash and a double one is the same mount", step: "RUN --mount=type=cache,target=/var/cache//apt/ apt-get install -y curl",
			cmd: "apt-get install -y curl", mounts: []mnt{"/var/cache/apt"}, earlier: "rm -f /etc/apt/apt.conf.d/docker-clean",
			want: "RUN --mount=type=cache,target=/var/cache//apt/,sharing=locked \\\n    --mount=type=cache,target=/var/lib/apt,sharing=locked \\\n    apt-get install -y curl"},
		{name: "a flag on a continued line is dropped", step: "RUN pip install \\\n  --no-cache-dir -r r.txt", cmd: "pip install --no-cache-dir -r r.txt",
			want: "RUN --mount=type=cache,target=/root/.cache/pip pip install \\\n  -r r.txt"},
		{name: "not a RUN", step: "COPY . .", cmd: "npm ci", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var anc []json.RawMessage
			if c.earlier != "" {
				anc = append(anc, op(t, c.earlier, nil, "", "/"))
			}
			hs := Analyze(op(t, c.cmd, c.env, c.user, "/app", c.mounts...), anc)
			if got := Fragment(c.step, hs); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}
