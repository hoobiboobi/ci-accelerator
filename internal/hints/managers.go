package hints

import (
	"path"
	"slices"
	"strings"
)

// manager is one package manager's rules: which commands run it, where
// its cache is, what switches the cache off or empties it, what it
// installs into (a cache mount there loses it from the image), and the
// caveats of a cache mount for it.
type manager struct {
	name        string
	match       func(command) bool
	caches      []cacheDir
	locked      bool // system package managers: one at a time per cache
	pathCommand string
	offFlags    []string
	offEnv      []string
	wipes       func(command) string
	targets     []func(*context, command) string
	outputs     []func(*context, command) string
	extra       func(x *context, uses []command, h *Hint, covered bool)
	caveats     []string
}

// cacheDir is where a manager keeps one cache, in order of precedence:
// a flag, variables, then a default (absolute, under the working folder,
// under $XDG_CACHE_HOME or ~/.cache, or under the home folder).
type cacheDir struct {
	label   string
	flags   []string
	env     []envVar
	abs     string
	cwd     string
	xdg     string
	home    string
	id      string // mount id when the folder is project-relative
	need    func(command) bool
	markers []string // path endings that make a mount look like this cache
	// also are folders that hold the cache too in this build (apk's
	// /var/cache/apk once /etc/apk/cache links to it).
	also func(*context) []string
}

type envVar struct {
	name   string
	suffix string
	list   bool // a list of folders: the first is used
}

func (m *manager) marks(d cacheDir, dest string) bool {
	for _, k := range d.markers {
		if strings.HasSuffix(dest, k) || strings.Contains(dest, k+"/") {
			return true
		}
	}
	return false
}

func (m *manager) dropFlag(off string) bool {
	return slices.Contains(m.offFlags, off)
}

// is matches a command by program and, when subs is not empty, by its
// first argument that is not an option.
func is(progs []string, subs ...string) func(command) bool {
	return func(c command) bool {
		if !slices.Contains(progs, c.prog) {
			return false
		}
		if len(subs) == 0 {
			return true
		}
		s := c.sub(1)
		return len(s) == 1 && slices.Contains(subs, s[0])
	}
}

// wipe matches a command that empties a cache: a program and the words
// that must follow it.
func wipe(prog string, words ...string) func(command) string {
	return func(c command) string {
		if c.prog != prog {
			return ""
		}
		for _, w := range words {
			if !slices.Contains(c.args, w) {
				return ""
			}
		}
		return strings.TrimSpace(prog + " " + strings.Join(words, " "))
	}
}

func anyWipe(fs ...func(command) string) func(command) string {
	return func(c command) string {
		for _, f := range fs {
			if w := f(c); w != "" {
				return w
			}
		}
		return ""
	}
}

// rmOf matches "rm -rf <folder>..." of a folder inside dir.
func rmOf(dir string) func(command) string {
	return func(c command) string {
		if c.prog != "rm" {
			return ""
		}
		for _, a := range c.args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			p := strings.TrimSuffix(strings.TrimSuffix(a, "*"), "/")
			if !strings.HasPrefix(p, "/") {
				p = path.Join(c.cwd, p)
			}
			if within(path.Clean(p), dir) {
				return "rm " + a
			}
		}
		return ""
	}
}

// inCwd is a project folder of a command.
func inCwd(rel string) func(*context, command) string {
	return func(_ *context, c command) string { return path.Join(c.cwd, rel) }
}

// envPath is a folder a variable names, if set to a plain path.
func envPath(name string) func(*context, command) string {
	return func(x *context, c command) string { return x.abs(c.env[name], c.cwd) }
}

// flagPath is a folder a flag names.
func flagPath(flag string) func(*context, command) string {
	return func(x *context, c command) string {
		v, ok := c.value(flag)
		if !ok {
			return ""
		}
		return x.abs(v, c.cwd)
	}
}

var (
	python = func(c command) bool {
		return c.prog == "python" && len(c.args) >= 3 && c.args[0] == "-m" && c.args[1] == "pip"
	}
	pipInstall = func(c command) bool {
		var rest []string
		switch {
		case c.prog == "pip":
			rest = c.args
		case python(c):
			rest = c.args[2:]
		default:
			return false
		}
		s := command{args: rest}.sub(1)
		return len(s) == 1 && (s[0] == "install" || s[0] == "download" || s[0] == "wheel")
	}
)

// managers in the order a step's hints are listed.
var managers = []*manager{
	{
		name:  "apt",
		match: is([]string{"apt-get", "apt", "aptitude"}, "install", "upgrade", "dist-upgrade", "full-upgrade", "build-dep", "source", "reinstall"),
		caches: []cacheDir{
			{label: "packages", abs: "/var/cache/apt", markers: []string{"/var/cache/apt"}},
			{label: "package lists", abs: "/var/lib/apt", markers: []string{"/var/lib/apt"}},
		},
		locked: true,
		wipes: anyWipe(wipe("apt-get", "clean"), wipe("apt", "clean"), wipe("apt-get", "autoclean"),
			rmOf("/var/cache/apt"), rmOf("/var/lib/apt")),
		extra: func(x *context, uses []command, h *Hint, covered bool) {
			// Debian and Ubuntu images delete every downloaded package
			// after an install (/etc/apt/apt.conf.d/docker-clean), and the
			// apt command deletes them unless Keep-Downloaded-Packages is
			// set: a cache mount then stays empty.
			if x.mentions("docker-clean") && (x.mentions("Keep-Downloaded-Packages") || !x.runs(func(c command) bool { return c.prog == "apt" })) {
				return
			}
			h.Before = append(h.Before, `RUN rm -f /etc/apt/apt.conf.d/docker-clean; echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache`)
			if covered {
				h.Problems = append(h.Problems, Problem{Kind: Disabled, Setting: "docker-clean"})
			}
		},
		caveats: []string{
			"Debian and Ubuntu images delete downloaded packages after every install (/etc/apt/apt.conf.d/docker-clean): remove that file in an earlier step, or the cache mount stays empty.",
			"sharing=locked: apt allows one run at a time on its cache; two builds sharing it at once fail on apt's lock.",
		},
	},
	{
		name:  "apk",
		match: is([]string{"apk"}, "add", "upgrade", "fix"),
		caches: []cacheDir{
			{label: "packages", abs: "/etc/apk/cache", markers: []string{"/etc/apk/cache", "/var/cache/apk"},
				also: func(x *context) []string {
					// apk's own advice: ln -s /var/cache/apk /etc/apk/cache.
					if x.mentions("/etc/apk/cache") {
						return []string{"/var/cache/apk"}
					}
					return nil
				}},
		},
		locked:   true,
		offFlags: []string{"--no-cache"},
		wipes:    anyWipe(wipe("apk", "cache", "clean"), wipe("apk", "cache", "purge"), rmOf("/etc/apk/cache")),
		caveats: []string{
			"apk keeps packages only when /etc/apk/cache exists; a cache mount there creates it. Drop --no-cache: with the mount the cache is not in the image anyway.",
		},
	},
	{
		name:  "dnf",
		match: is([]string{"dnf", "dnf5", "yum", "microdnf", "tdnf"}, "install", "upgrade", "update", "reinstall", "groupinstall", "group", "builddep", "distro-sync", "makecache", "module", "localinstall"),
		caches: []cacheDir{
			{label: "packages", abs: "/var/cache/dnf", markers: []string{"/var/cache/dnf", "/var/cache/libdnf5", "/var/cache/yum", "/var/cache/tdnf"},
				need: func(c command) bool { return c.prog == "dnf" }},
			{label: "packages (dnf5)", abs: "/var/cache/libdnf5", markers: []string{"/var/cache/libdnf5", "/var/cache/dnf"},
				need: func(c command) bool { return c.prog == "dnf5" || c.prog == "dnf" }},
			// microdnf (UBI minimal images) keeps its cache where yum does.
			{label: "packages", abs: "/var/cache/yum", markers: []string{"/var/cache/yum"},
				need: func(c command) bool { return c.prog == "yum" || c.prog == "microdnf" }},
			{label: "packages", abs: "/var/cache/tdnf", markers: []string{"/var/cache/tdnf"},
				need: func(c command) bool { return c.prog == "tdnf" }},
		},
		locked: true,
		wipes: anyWipe(wipe("dnf", "clean"), wipe("dnf5", "clean"), wipe("yum", "clean"), wipe("microdnf", "clean"), wipe("tdnf", "clean"),
			rmOf("/var/cache/dnf"), rmOf("/var/cache/yum"), rmOf("/var/cache/libdnf5")),
		extra: func(x *context, uses []command, h *Hint, covered bool) {
			// dnf and yum delete downloaded packages after a successful
			// install unless keepcache is on.
			if x.mentions("keepcache=1", "keepcache=True", "keepcache=true", "keepcache=yes", "keepcache=on") {
				return
			}
			for _, c := range uses {
				h.Insert = append(h.Insert, Insert{After: c.prog, Add: "--setopt=keepcache=1"})
			}
			if covered {
				h.Problems = append(h.Problems, Problem{Kind: Disabled, Setting: "keepcache"})
			}
		},
		caveats: []string{
			"dnf and yum delete downloaded packages after an install unless keepcache is on: --setopt=keepcache=1, or keepcache=1 in the configuration.",
			"Fedora 41 and later run dnf5, which keeps its cache in /var/cache/libdnf5; mounting both folders covers either version.",
		},
	},
	{
		name:  "zypper",
		match: is([]string{"zypper"}, "install", "in", "update", "up", "dist-upgrade", "dup", "patch", "source-install", "si"),
		caches: []cacheDir{
			{label: "packages", abs: "/var/cache/zypp", markers: []string{"/var/cache/zypp"}},
		},
		locked: true,
		wipes:  anyWipe(wipe("zypper", "clean"), wipe("zypper", "cc"), rmOf("/var/cache/zypp")),
		extra: func(x *context, uses []command, h *Hint, covered bool) {
			if x.mentions("keep-packages", "keeppackages") {
				return
			}
			h.Before = append(h.Before, "RUN zypper modifyrepo --keep-packages --all")
			if covered {
				h.Problems = append(h.Problems, Problem{Kind: Disabled, Setting: "keeppackages"})
			}
		},
		caveats: []string{
			"zypper keeps downloaded packages only for repositories with keeppackages on (zypper modifyrepo --keep-packages --all); repositories added later need it too.",
		},
	},
	{
		name:  "pip",
		match: pipInstall,
		caches: []cacheDir{
			{label: "cache", flags: []string{"--cache-dir"}, env: []envVar{{name: "PIP_CACHE_DIR"}}, xdg: "pip", markers: []string{"/.cache/pip", "/pip"}},
		},
		pathCommand: "pip cache dir",
		offFlags:    []string{"--no-cache-dir"},
		offEnv:      []string{"PIP_NO_CACHE_DIR"},
		wipes:       anyWipe(wipe("pip", "cache", "purge")),
		targets:     []func(*context, command) string{flagPath("--target"), flagPath("-t"), flagPath("--prefix"), flagPath("--root")},
		caveats: []string{
			"Drop --no-cache-dir (or PIP_NO_CACHE_DIR): it keeps the cache out of the image, and a cache mount is not in the image anyway.",
			"Mount the cache, never site-packages or a --target folder: what is in a cache mount is not part of the image.",
		},
	},
	{
		name: "uv",
		match: func(c command) bool {
			if c.prog != "uv" {
				return false
			}
			s := c.sub(2)
			if len(s) == 0 {
				return false
			}
			switch s[0] {
			case "sync", "add", "run", "lock":
				return true
			case "pip":
				return len(s) == 2 && (s[1] == "install" || s[1] == "sync")
			case "tool":
				return len(s) == 2 && s[1] == "install"
			}
			return false
		},
		caches: []cacheDir{
			{label: "cache", flags: []string{"--cache-dir"}, env: []envVar{{name: "UV_CACHE_DIR"}}, xdg: "uv", markers: []string{"/.cache/uv", "/uv"}},
		},
		pathCommand: "uv cache dir",
		offFlags:    []string{"--no-cache", "-n"},
		offEnv:      []string{"UV_NO_CACHE"},
		wipes:       anyWipe(wipe("uv", "cache", "clean"), wipe("uv", "cache", "prune")),
		targets:     []func(*context, command) string{inCwd(".venv"), envPath("UV_PROJECT_ENVIRONMENT"), flagPath("--target")},
		extra: func(x *context, uses []command, h *Hint, covered bool) {
			if uses[0].env["UV_LINK_MODE"] == "" {
				h.Before = append(h.Before, "ENV UV_LINK_MODE=copy")
			}
		},
		caveats: []string{
			"Set UV_LINK_MODE=copy: the cache mount is another file system, so uv cannot link from it and warns.",
			"Mount the cache, never .venv: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "poetry",
		match: is([]string{"poetry"}, "install", "add", "update", "sync", "lock"),
		caches: []cacheDir{
			{label: "downloads", env: []envVar{{name: "POETRY_CACHE_DIR", suffix: "/cache"}}, xdg: "pypoetry/cache", markers: []string{"/pypoetry/cache", "/pypoetry"}},
			{label: "artifacts", env: []envVar{{name: "POETRY_CACHE_DIR", suffix: "/artifacts"}}, xdg: "pypoetry/artifacts", markers: []string{"/pypoetry/artifacts", "/pypoetry"}},
		},
		pathCommand: "poetry config cache-dir",
		offFlags:    []string{"--no-cache"},
		wipes:       anyWipe(wipe("poetry", "cache", "clear")),
		targets: []func(*context, command) string{inCwd(".venv"), func(x *context, c command) string {
			// Poetry creates its virtual environments inside its cache
			// folder unless told to put them in the project or not to
			// create one.
			if !truthy(c.env["POETRY_VIRTUALENVS_CREATE"]) && c.env["POETRY_VIRTUALENVS_CREATE"] != "" || truthy(c.env["POETRY_VIRTUALENVS_IN_PROJECT"]) {
				return ""
			}
			if d := x.abs(c.env["POETRY_CACHE_DIR"], c.cwd); d != "" {
				return d + "/virtualenvs"
			}
			if v := c.env["XDG_CACHE_HOME"]; strings.HasPrefix(v, "/") {
				return v + "/pypoetry/virtualenvs"
			}
			if x.home != "" {
				return x.home + "/.cache/pypoetry/virtualenvs"
			}
			return ""
		}},
		caveats: []string{
			"Poetry keeps its virtual environments in its cache folder (unless virtualenvs.in-project or virtualenvs.create=false): mount its cache/ and artifacts/ folders, not the whole cache folder, or the environment is not in the image.",
		},
	},
	{
		name:  "conda",
		match: is([]string{"conda", "mamba", "micromamba"}, "install", "create", "update", "env", "upgrade"),
		caches: []cacheDir{
			{label: "package cache", env: []envVar{{name: "CONDA_PKGS_DIRS", list: true}, {name: "MAMBA_ROOT_PREFIX", suffix: "/pkgs"}, {name: "CONDA_DIR", suffix: "/pkgs"}},
				markers: []string{"/pkgs"}},
		},
		pathCommand: "conda config --show pkgs_dirs",
		wipes:       anyWipe(wipe("conda", "clean"), wipe("mamba", "clean"), wipe("micromamba", "clean")),
		caveats: []string{
			"Drop conda clean (or micromamba clean) from the step: it empties the cache mount that keeps the packages.",
			"Packages are copied from the cache into the environment, since the cache mount is another file system.",
		},
	},
	{
		name:  "npm",
		match: is([]string{"npm"}, "ci", "install", "i", "in", "isntall", "add", "update", "up", "install-ci-test", "install-test", "it", "cit"),
		caches: []cacheDir{
			{label: "cache", flags: []string{"--cache"}, env: []envVar{{name: "npm_config_cache"}, {name: "NPM_CONFIG_CACHE"}}, home: ".npm", markers: []string{"/.npm"}},
		},
		pathCommand: "npm config get cache",
		wipes:       anyWipe(wipe("npm", "cache", "clean")),
		targets:     []func(*context, command) string{inCwd("node_modules")},
		caveats: []string{
			"Mount the npm cache, never node_modules: what is in a cache mount is not part of the image.",
		},
	},
	{
		name: "yarn",
		match: func(c command) bool {
			if c.prog != "yarn" {
				return false
			}
			s := c.sub(1)
			return len(s) == 0 || s[0] == "install" || s[0] == "add" || s[0] == "upgrade" || s[0] == "up"
		},
		caches: []cacheDir{
			{label: "cache", flags: []string{"--cache-folder"}, env: []envVar{{name: "YARN_CACHE_FOLDER"}}, markers: []string{"/.cache/yarn", "/.yarn/berry/cache", "/yarn"}},
		},
		pathCommand: "yarn cache dir (Yarn 1) or yarn config get cacheFolder (Yarn 2 and later)",
		wipes:       anyWipe(wipe("yarn", "cache", "clean")),
		targets:     []func(*context, command) string{inCwd("node_modules")},
		caveats: []string{
			"Where Yarn keeps its cache depends on its version: Yarn 1 uses ~/.cache/yarn, Yarn 4 ~/.yarn/berry/cache, Yarn 2 and 3 the project's .yarn/cache. Setting YARN_CACHE_FOLDER in the step makes it one known folder.",
			"Mount the cache, never node_modules: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "pnpm",
		match: is([]string{"pnpm"}, "install", "i", "add", "fetch", "update", "up", "ci"),
		caches: []cacheDir{
			{label: "store", flags: []string{"--store-dir"}, env: []envVar{{name: "npm_config_store_dir"}, {name: "PNPM_STORE_DIR"}, {name: "PNPM_HOME", suffix: "/store"}},
				home: ".local/share/pnpm/store", markers: []string{"/pnpm/store", "/.pnpm-store"}},
		},
		pathCommand: "pnpm store path",
		wipes:       anyWipe(wipe("pnpm", "store", "prune")),
		targets:     []func(*context, command) string{inCwd("node_modules")},
		caveats: []string{
			"pnpm links packages from its store; from a cache mount (another file system) it copies them instead, which still saves the downloads.",
			"Mount the store, never node_modules: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "bun",
		match: is([]string{"bun"}, "install", "i", "add"),
		caches: []cacheDir{
			{label: "cache", env: []envVar{{name: "BUN_INSTALL_CACHE_DIR"}, {name: "BUN_INSTALL", suffix: "/install/cache"}}, home: ".bun/install/cache", markers: []string{"/install/cache"}},
		},
		pathCommand: "bun pm cache",
		offFlags:    []string{"--no-cache"},
		wipes:       anyWipe(wipe("bun", "pm", "cache", "rm")),
		targets:     []func(*context, command) string{inCwd("node_modules")},
		caveats: []string{
			"Mount Bun's cache, never node_modules: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "go",
		match: is([]string{"go"}, "build", "install", "test", "vet", "run", "get", "mod", "list", "generate"),
		caches: []cacheDir{
			{label: "module cache", env: []envVar{{name: "GOMODCACHE"}, {name: "GOPATH", suffix: "/pkg/mod", list: true}}, home: "go/pkg/mod", markers: []string{"/pkg/mod"},
				need: func(c command) bool {
					return !c.arg("-mod=vendor") && !strings.Contains(c.env["GOFLAGS"], "-mod=vendor")
				}},
			{label: "build cache", env: []envVar{{name: "GOCACHE"}}, xdg: "go-build", markers: []string{"/go-build"},
				need: func(c command) bool {
					s := c.sub(1)
					return len(s) == 1 && (s[0] == "build" || s[0] == "install" || s[0] == "test" || s[0] == "vet" || s[0] == "run")
				}},
		},
		pathCommand: "go env GOMODCACHE GOCACHE",
		wipes: func(c command) string {
			if c.prog == "go" && len(c.args) > 0 && c.args[0] == "clean" && (c.arg("-cache") || c.arg("-modcache")) {
				return "go clean " + strings.Join(c.args[1:], " ")
			}
			return ""
		},
		targets: []func(*context, command) string{envPath("GOBIN"), func(x *context, c command) string {
			if p := c.env["GOPATH"]; p != "" && c.prog == "go" && len(c.sub(1)) == 1 && c.sub(1)[0] == "install" {
				first, _, _ := strings.Cut(p, ":")
				return x.abs(first+"/bin", c.cwd)
			}
			return ""
		}},
		caveats: []string{
			"Mount the module cache and the build cache, not the folder go install writes to: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "cargo",
		match: is([]string{"cargo"}, "build", "install", "fetch", "test", "run", "check", "doc", "clippy", "update", "vendor", "b", "t", "r", "c"),
		caches: []cacheDir{
			{label: "registry", env: []envVar{{name: "CARGO_HOME", suffix: "/registry"}}, home: ".cargo/registry", markers: []string{"/registry"}},
			{label: "git checkouts", env: []envVar{{name: "CARGO_HOME", suffix: "/git"}}, home: ".cargo/git", markers: []string{"cargo/git", "/git/db"}},
		},
		pathCommand: "echo ${CARGO_HOME:-$HOME/.cargo}",
		targets: []func(*context, command) string{func(x *context, c command) string {
			if len(c.sub(1)) == 1 && c.sub(1)[0] == "install" {
				if r, ok := c.value("--root"); ok {
					return x.abs(r, c.cwd)
				}
				if h := c.env["CARGO_HOME"]; h != "" {
					return x.abs(h+"/bin", c.cwd)
				}
			}
			return ""
		}},
		outputs: []func(*context, command) string{envPath("CARGO_TARGET_DIR"), flagPath("--target-dir"), inCwd("target")},
		caveats: []string{
			"A cache mount on target/ also speeds up compiling, but the binary must be copied out of it in the same step (cp target/release/app /usr/local/bin/): what is in a cache mount is not part of the image.",
		},
	},
	{
		name: "maven",
		match: func(c command) bool {
			return c.prog == "mvn" || c.prog == "mvnw" || c.prog == "mvnd"
		},
		caches: []cacheDir{
			{label: "local repository", flags: []string{"-Dmaven.repo.local="}, home: ".m2/repository", markers: []string{"/.m2/repository", "/.m2"}},
		},
		pathCommand: "mvn help:evaluate -Dexpression=settings.localRepository -q -DforceStdout",
		wipes: func(c command) string {
			if (c.prog == "mvn" || c.prog == "mvnw") && c.arg("dependency:purge-local-repository") {
				return "dependency:purge-local-repository"
			}
			return ""
		},
		caveats: []string{
			"Mount the local repository (~/.m2/repository), not ~/.m2 as a whole if settings.xml is copied there by the Dockerfile: a cache mount hides what the image has at its place.",
			"The downloaded artifacts are only in the cache mount, not in the image: a later step that runs mvn (also offline) needs the same mount.",
		},
	},
	{
		name: "gradle",
		match: func(c command) bool {
			return c.prog == "gradle" || c.prog == "gradlew"
		},
		caches: []cacheDir{
			{label: "dependency cache", flags: []string{"--gradle-user-home", "-g"}, env: []envVar{{name: "GRADLE_USER_HOME", suffix: "/caches"}}, home: ".gradle/caches", markers: []string{"/.gradle/caches", "/.gradle", "/caches"}},
			{label: "Gradle distributions", flags: []string{}, env: []envVar{{name: "GRADLE_USER_HOME", suffix: "/wrapper"}}, home: ".gradle/wrapper", markers: []string{"/.gradle/wrapper"},
				need: func(c command) bool { return c.prog == "gradlew" }},
		},
		pathCommand: "echo ${GRADLE_USER_HOME:-$HOME/.gradle}",
		caveats: []string{
			"Gradle locks its caches itself, so builds can share the mount at once.",
		},
	},
	{
		name:  "sbt",
		match: is([]string{"sbt"}),
		caches: []cacheDir{
			{label: "Coursier cache", env: []envVar{{name: "COURSIER_CACHE"}}, xdg: "coursier", markers: []string{"/coursier"}},
			{label: "Ivy cache", home: ".ivy2/cache", markers: []string{"/.ivy2"}},
			{label: "sbt boot", home: ".sbt", markers: []string{"/.sbt"}},
		},
		pathCommand: "cs about (Coursier) or echo $HOME",
	},
	{
		name:  "nuget",
		match: is([]string{"dotnet"}, "restore", "build", "publish", "test", "pack", "run"),
		caches: []cacheDir{
			{label: "packages", flags: []string{"--packages"}, env: []envVar{{name: "NUGET_PACKAGES"}}, home: ".nuget/packages", markers: []string{"/.nuget/packages", "/.nuget"}},
		},
		pathCommand: "dotnet nuget locals global-packages --list",
		wipes: func(c command) string {
			if c.prog == "dotnet" && c.arg("locals") && (c.arg("--clear") || c.arg("-c")) {
				return "dotnet nuget locals --clear"
			}
			return ""
		},
		caveats: []string{
			"The packages are only in the cache mount, not in the image: every later step that builds (dotnet build, publish, test, also with --no-restore) needs the same mount.",
			"NuGet coordinates concurrent restores with lock files in its temp folder (/tmp/NuGetScratch...), which is not shared between builds: sharing=locked is the safe choice for a mount several builds use at once.",
		},
	},
	{
		name:  "composer",
		match: is([]string{"composer"}, "install", "update", "require", "create-project", "i", "u"),
		caches: []cacheDir{
			{label: "cache", env: []envVar{{name: "COMPOSER_CACHE_DIR"}, {name: "COMPOSER_HOME", suffix: "/cache"}}, markers: []string{"/composer", "/cache"}},
		},
		pathCommand: "composer config --global cache-dir",
		offFlags:    []string{"--no-cache"},
		wipes:       anyWipe(wipe("composer", "clear-cache"), wipe("composer", "clearcache"), wipe("composer", "cc")),
		targets:     []func(*context, command) string{inCwd("vendor")},
		caveats: []string{
			"Mount Composer's cache, never vendor/: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "bundler",
		match: is([]string{"bundle", "gem"}, "install"),
		caches: []cacheDir{
			{label: "gem cache", env: []envVar{{name: "GEM_HOME", suffix: "/cache"}}, markers: []string{"/cache"}},
		},
		pathCommand: "gem env gemdir (its cache/ folder)",
		targets: []func(*context, command) string{envPath("BUNDLE_PATH"), func(x *context, c command) string {
			if p := c.env["GEM_HOME"]; p != "" {
				return x.abs(p, c.cwd)
			}
			return ""
		}, inCwd("vendor/bundle")},
		caveats: []string{
			"Mount only the cache/ folder of the gem home, never the gem home or BUNDLE_PATH itself: the installed gems must stay in the image.",
		},
	},
	{
		name: "pub",
		match: func(c command) bool {
			if c.prog != "dart" && c.prog != "flutter" {
				return false
			}
			s := c.sub(2)
			return len(s) == 2 && s[0] == "pub" && (s[1] == "get" || s[1] == "upgrade" || s[1] == "downgrade")
		},
		caches: []cacheDir{
			{label: "pub cache", env: []envVar{{name: "PUB_CACHE"}}, home: ".pub-cache", markers: []string{"/.pub-cache", "/pub-cache"}},
		},
		pathCommand: "echo ${PUB_CACHE:-$HOME/.pub-cache}",
		caveats: []string{
			"Dart resolves packages from the pub cache when the app runs: compile it (dart compile exe) in the same or a later step that has the mount, or the image needs the cache itself.",
		},
	},
	{
		name: "hex",
		match: func(c command) bool {
			if c.prog != "mix" {
				return false
			}
			s := c.sub(1)
			return len(s) == 1 && (s[0] == "deps.get" || s[0] == "deps.update" || strings.HasPrefix(s[0], "hex."))
		},
		caches: []cacheDir{
			{label: "Hex packages", env: []envVar{{name: "HEX_HOME", suffix: "/packages"}}, home: ".hex/packages", markers: []string{"/.hex/packages", "/.hex"}},
		},
		pathCommand: "mix hex.config cache_dir",
		caveats: []string{
			"Mount only Hex's packages folder: ~/.mix holds Hex and rebar themselves, which later steps need.",
		},
	},
	{
		name:  "stack",
		match: is([]string{"stack"}, "build", "install", "setup", "test", "bench"),
		caches: []cacheDir{
			{label: "Stack root", flags: []string{"--stack-root"}, env: []envVar{{name: "STACK_ROOT"}}, home: ".stack", markers: []string{"/.stack"}},
		},
		pathCommand: "stack path --stack-root",
		caveats: []string{
			"The Stack root holds the compiler too: copy the built program out (stack install --local-bin-path) in the same step.",
		},
	},
	{
		name:  "cabal",
		match: is([]string{"cabal"}, "update", "build", "install", "v2-build", "v2-install", "v2-update", "test"),
		caches: []cacheDir{
			{label: "package cache", env: []envVar{{name: "CABAL_DIR", suffix: "/packages"}}, markers: []string{"/.cabal/packages", "/cabal/packages", "/.cabal"}},
		},
		pathCommand: "cabal path --cache-dir",
		caveats: []string{
			"Where cabal keeps its downloads depends on its version (~/.cabal/packages, or ~/.cache/cabal/packages since 3.10): ask cabal path.",
		},
	},
	{
		name: "swift",
		match: func(c command) bool {
			if c.prog != "swift" {
				return false
			}
			s := c.sub(2)
			return len(s) >= 1 && (s[0] == "build" || s[0] == "test" || s[0] == "package" && len(s) == 2 && (s[1] == "resolve" || s[1] == "update"))
		},
		caches: []cacheDir{
			{label: "package cache", flags: []string{"--cache-path"}, xdg: "org.swift.swiftpm", markers: []string{"/org.swift.swiftpm"}},
		},
		pathCommand: "ls -d ~/.cache/org.swift.swiftpm",
		outputs:     []func(*context, command) string{flagPath("--scratch-path"), inCwd(".build")},
		caveats: []string{
			"A cache mount on .build also speeds up compiling, but the binary must be copied out of it in the same step: what is in a cache mount is not part of the image.",
		},
	},
	{
		name:  "deno",
		match: is([]string{"deno"}, "install", "cache", "compile", "test", "check"),
		caches: []cacheDir{
			{label: "cache", env: []envVar{{name: "DENO_DIR"}}, xdg: "deno", markers: []string{"/deno", "/deno-dir"}},
		},
		pathCommand: "deno info",
		caveats: []string{
			"An app started with deno run needs its modules in the image: compile it (deno compile) or keep DENO_DIR out of the cache mount.",
		},
	},
	{
		name: "ccache",
		match: func(c command) bool {
			return c.prog == "ccache" || strings.Contains(c.env["CC"]+" "+c.env["CXX"]+" "+c.env["CMAKE_C_COMPILER_LAUNCHER"]+" "+c.env["CMAKE_CXX_COMPILER_LAUNCHER"], "ccache") && builds(c) ||
				builds(c) && slices.ContainsFunc(c.args, func(a string) bool { return strings.Contains(a, "COMPILER_LAUNCHER=ccache") })
		},
		caches: []cacheDir{
			{label: "ccache", env: []envVar{{name: "CCACHE_DIR"}}, markers: []string{"ccache"}},
		},
		pathCommand: "ccache --get-config cache_dir",
		caveats: []string{
			"ccache uses ~/.ccache when that folder exists and ~/.cache/ccache otherwise; CCACHE_DIR makes it one known folder.",
		},
	},
	{
		name: "sccache",
		match: func(c command) bool {
			return c.prog == "sccache" || strings.Contains(c.env["RUSTC_WRAPPER"]+" "+c.env["CC"]+" "+c.env["CMAKE_C_COMPILER_LAUNCHER"], "sccache") && builds(c)
		},
		caches: []cacheDir{
			{label: "sccache", env: []envVar{{name: "SCCACHE_DIR"}}, xdg: "sccache", markers: []string{"sccache"}},
		},
		pathCommand: "sccache --show-stats (Cache location)",
	},
	{
		name: "next",
		match: func(c command) bool {
			return c.prog == "next" && len(c.sub(1)) == 1 && c.sub(1)[0] == "build"
		},
		caches: []cacheDir{
			{label: "build cache", cwd: ".next/cache", id: "next-cache", markers: []string{"/.next/cache"}},
		},
		pathCommand: "",
		targets:     []func(*context, command) string{inCwd(".next")},
		caveats: []string{
			"Mount only .next/cache: the rest of .next is the built app the image needs.",
			"The mount needs an id (as in the fragment) when several projects build with the same working folder, or they share one cache.",
		},
	},
	{
		name: "turbo",
		match: func(c command) bool {
			return c.prog == "turbo" && len(c.sub(1)) == 1 && (c.sub(1)[0] == "run" || c.sub(1)[0] == "build")
		},
		caches: []cacheDir{
			{label: "local cache", flags: []string{"--cache-dir"}, env: []envVar{{name: "TURBO_CACHE_DIR"}}, markers: []string{"/.turbo", "/turbo"}},
		},
		pathCommand: "",
		caveats: []string{
			"Turborepo 2 keeps its local cache in .turbo/cache, earlier versions in node_modules/.cache/turbo; --cache-dir (or TURBO_CACHE_DIR) makes it one known folder.",
		},
	},
	{
		name: "nx",
		match: func(c command) bool {
			s := c.sub(1)
			return c.prog == "nx" && len(s) == 1 && (s[0] == "build" || s[0] == "run" || s[0] == "run-many" || s[0] == "affected" || strings.Contains(s[0], ":"))
		},
		caches: []cacheDir{
			{label: "local cache", env: []envVar{{name: "NX_CACHE_DIRECTORY"}}, markers: []string{"/.nx", "/cache/nx"}},
		},
		pathCommand: "",
		caveats: []string{
			"Where Nx keeps its local cache depends on its version (recent versions: a folder under ~/.nx); NX_CACHE_DIRECTORY makes it one known folder.",
		},
	},
}

// builds reports whether a command compiles (for compiler caches, which
// only work around a compiler run).
func builds(c command) bool {
	switch c.prog {
	case "make", "cmake", "ninja", "meson", "cc", "gcc", "g++", "c++", "clang", "clang++", "cargo", "bazel", "scons":
		return true
	}
	return false
}
