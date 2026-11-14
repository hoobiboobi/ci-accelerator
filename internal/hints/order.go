package hints

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

// Install is a command of a RUN step that installs dependencies a
// manager reads from files of the project: "pip install .", "npm ci",
// "go mod download". A step that runs one after copying the whole build
// context runs it again whenever any file changes, though only those
// files decide what it installs.
type Install struct {
	Manager string `json:"manager"`
	// Command is the command as the Dockerfile wrote it.
	Command string `json:"command"`
	// Dir is the folder the manager reads its files from, in the image.
	Dir string `json:"dir"`
	// Files are the names of those files in Dir; a name ending in "*" is
	// copied when it is there (a lock file the project may not have).
	Files []string `json:"files"`
	// Project: the command installs the project itself too (pip install .,
	// uv sync, composer install), which needs the code.
	Project bool `json:"project,omitempty"`
}

// installer is one manager's install commands: which commands install
// from files of the project, which files, and how to split one into the
// dependencies alone (before the code is copied) and the rest (after).
type installer struct {
	manager string
	// match returns the files the command reads, relative to its folder,
	// and whether it installs the project itself; ok false when the
	// command is not an install of this kind.
	match func(c command) (files []string, project bool, ok bool)
	// dir is the project folder of a matched command (default: its cwd).
	dir func(c command) string
	// before is the command that installs the dependencies alone, from
	// the files only; after what still runs once the code is there ("" to
	// drop the command there). ok false when the manager offers no way.
	split func(text string, c command, x *orderCtx) (before, after string, ok bool)
	// deep are files the manager reads in every folder below its own (a
	// workspace's package.json files), copied with COPY --parents.
	deep []string
	// names are the base names of the files a change to which reinstalls
	// for good reason: a changed file with one of these names anywhere in
	// the context counts (a workspace's package.json in a sub-folder too).
	names []string
}

// orderCtx is what a split may need beyond the command: the step's
// environment (an image's PYTHON_VERSION).
type orderCtx struct {
	env map[string]string
}

var installers = []installer{
	{
		manager: "pip",
		match: func(c command) ([]string, bool, bool) {
			if !pipInstallsOnly(c) || localPaths(c) {
				return nil, false, false
			}
			files, project := pipFiles(c)
			return files, project, true
		},
		split: func(text string, c command, x *orderCtx) (string, string, bool) {
			files, project := pipFiles(c)
			if !project {
				if len(files) == 0 {
					// Named packages only: nothing of the project.
					return text, "", true
				}
				// A requirements file may name the project itself ("-e .")
				// or other files: the early install takes the rest of it,
				// and the command runs again once the code is there.
				before, n := text, 0
				var pre []string
				for _, f := range files {
					tmp := "/tmp/requirements-" + strconv.Itoa(n) + ".txt"
					n++
					pre = append(pre, "sed -E '/^[[:space:]]*(-e|--editable|-r|--requirement|-c|--constraint|[.]|[/]|file:)/d' "+f+" > "+tmp)
					before = replaceWord(before, f, tmp)
				}
				return strings.Join(pre, " \\\n && ") + " \\\n && " + before, text, true
			}
			// pip has no "dependencies only" for a project: the list
			// comes from pyproject.toml, read with tomllib (Python 3.11 and
			// later), when the project declares it there.
			if !pythonAtLeast(x.env["PYTHON_VERSION"], 3, 11) {
				return "", "", false
			}
			target, extras, ok := pipTarget(c)
			if !ok {
				return "", "", false
			}
			before := pyprojectDeps(extras) + " \\\n && " + replaceWord(text, target, "-r /tmp/requirements.txt")
			// The command itself still runs once the code is there: it
			// finds the dependencies installed and adds the project. A
			// project whose dependencies are not in pyproject.toml
			// (dynamic, or setup.py) installs them there, as before.
			return before, text, true
		},
		names: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "requirements.in", "constraints.txt", "Pipfile", "Pipfile.lock"},
	},
	{
		manager: "uv",
		match: func(c command) ([]string, bool, bool) {
			s := c.sub(2)
			switch {
			case c.prog == "uv" && len(s) > 0 && s[0] == "sync":
				return []string{"pyproject.toml", "uv.lock*"}, !c.arg("--no-install-project"), true
			case c.prog == "uv" && len(s) > 1 && s[0] == "pip" && s[1] == "install" && !localPaths(c):
				files, project := pipFiles(c)
				return files, project, true
			}
			return nil, false, false
		},
		split: func(text string, c command, x *orderCtx) (string, string, bool) {
			if s := c.sub(1); s[0] == "sync" {
				if c.arg("--no-install-project") {
					return text, "", true
				}
				return insertAfterWord(text, "sync", "--no-install-project"), text, true
			}
			_, project := pipFiles(c)
			if !project {
				return text, "", true
			}
			target, extras, ok := pipTarget(c)
			if !ok {
				return "", "", false
			}
			with := "-r pyproject.toml"
			for _, e := range extras {
				with += " --extra " + e
			}
			return replaceWord(text, target, with), text, true
		},
		names: []string{"pyproject.toml", "uv.lock", "requirements.txt", "requirements.in"},
	},
	{
		manager: "poetry",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "poetry" || !subIs(c, "install") {
				return nil, false, false
			}
			return []string{"pyproject.toml", "poetry.lock*"}, !c.arg("--no-root") && !c.arg("--only-root"), true
		},
		split: func(text string, c command, x *orderCtx) (string, string, bool) {
			if c.arg("--no-root") {
				return text, "", true
			}
			return insertAfterWord(text, "install", "--no-root"), text, true
		},
		names: []string{"pyproject.toml", "poetry.lock", "poetry.toml"},
	},
	{
		manager: "npm",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "npm" || !(subIs(c, "ci") || subIs(c, "install", "i") && len(c.sub(2)) == 1) {
				return nil, false, false
			}
			return []string{"package-lock.json*", "npm-shrinkwrap.json*", ".npmrc*"}, false, true
		},
		dir:   npmDir,
		deep:  []string{"package.json"},
		split: whole,
		names: []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", ".npmrc"},
	},
	{
		manager: "yarn",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "yarn" {
				return nil, false, false
			}
			if s := subSkipping(c, "--cwd", "--network-timeout", "--mutex", "--cache-folder", "--modules-folder", "--registry", "--network-concurrency", "--child-concurrency"); s != "" && s != "install" {
				return nil, false, false
			}
			return []string{"yarn.lock*", ".yarnrc*", ".yarnrc.yml*"}, false, true
		},
		dir:   yarnDir,
		deep:  []string{"package.json"},
		split: whole,
		names: []string{"package.json", "yarn.lock", ".yarnrc", ".yarnrc.yml"},
	},
	{
		manager: "pnpm",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "pnpm" || !subIs(c, "install", "i") || len(c.sub(2)) > 1 {
				return nil, false, false
			}
			return []string{"pnpm-lock.yaml*", "pnpm-workspace.yaml*", ".npmrc*"}, false, true
		},
		dir:   flagDir("-C", "--dir"),
		deep:  []string{"package.json"},
		split: whole,
		names: []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", ".npmrc", ".pnpmfile.cjs"},
	},
	{
		manager: "bun",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "bun" || !subIs(c, "install", "i") || len(c.sub(2)) > 1 {
				return nil, false, false
			}
			return []string{"bun.lock*", "bun.lockb*", "bunfig.toml*"}, false, true
		},
		dir:   flagDir("--cwd"),
		deep:  []string{"package.json"},
		split: whole,
		names: []string{"package.json", "bun.lock", "bun.lockb", "bunfig.toml"},
	},
	{
		manager: "go",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "go" || len(c.sub(2)) != 2 || c.sub(2)[0] != "mod" || c.sub(2)[1] != "download" {
				return nil, false, false
			}
			return []string{"go.mod", "go.sum*"}, false, true
		},
		split: whole,
		names: []string{"go.mod", "go.sum", "go.work", "go.work.sum"},
	},
	{
		manager: "bundler",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "bundle" || len(c.sub(1)) > 0 && !subIs(c, "install") {
				return nil, false, false
			}
			return []string{"Gemfile", "Gemfile.lock*"}, false, true
		},
		// A gem's Gemfile reads its gemspec, which reads the code (lib/x/
		// version.rb): installing from the Gemfile alone fails (basecamp/
		// kamal, lab 2026-11-14). Which Gemfile does is not in the record:
		// no fragment, the hint says it in words.
		split: func(string, command, *orderCtx) (string, string, bool) { return "", "", false },
		names: []string{"Gemfile", "Gemfile.lock", "gems.rb", "gems.locked"},
	},
	{
		manager: "composer",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "composer" || !subIs(c, "install") {
				return nil, false, false
			}
			return []string{"composer.json", "composer.lock*"}, !c.arg("--no-autoloader"), true
		},
		dir: flagDir("--working-dir", "-d"),
		split: func(text string, c command, x *orderCtx) (string, string, bool) {
			if c.arg("--no-autoloader") && c.arg("--no-scripts") {
				return text, "", true
			}
			// Without the code, the autoloader and the project's scripts
			// have nothing to work on: they run once the code is there.
			before := text
			for _, f := range []string{"--no-scripts", "--no-autoloader"} {
				if !c.arg(f) {
					before = insertAfterWord(before, "install", f)
				}
			}
			return before, text, true
		},
		names: []string{"composer.json", "composer.lock", "auth.json"},
	},
	{
		manager: "mix",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "mix" || !subIs(c, "deps.get") {
				return nil, false, false
			}
			return []string{"mix.exs", "mix.lock*"}, false, true
		},
		split: whole,
		names: []string{"mix.exs", "mix.lock"},
	},
	{
		// cargo fetch reads every member's Cargo.toml of a workspace and
		// needs their sources to exist: no fragment, the hint says it.
		manager: "cargo",
		match: func(c command) ([]string, bool, bool) {
			if c.prog != "cargo" || !subIs(c, "fetch") {
				return nil, false, false
			}
			return []string{"Cargo.toml", "Cargo.lock*"}, false, true
		},
		split: func(string, command, *orderCtx) (string, string, bool) { return "", "", false },
		names: []string{"Cargo.toml", "Cargo.lock", "config.toml"},
	},
}

// whole moves the command as it is: it reads nothing but the files.
func whole(text string, _ command, _ *orderCtx) (string, string, bool) { return text, "", true }

func subIs(c command, subs ...string) bool {
	s := c.sub(1)
	if len(s) == 0 {
		return false
	}
	for _, x := range subs {
		if s[0] == x {
			return true
		}
	}
	return false
}

// subSkipping is a command's first argument that is neither an option
// nor the value of one of the options that take one ("" when there is
// none): "install" for "yarn --network-timeout 600000 install".
func subSkipping(c command, valued ...string) string {
	for i := 0; i < len(c.args); i++ {
		a := c.args[i]
		if strings.HasPrefix(a, "-") {
			for _, v := range valued {
				if a == v {
					i++
				}
			}
			continue
		}
		return a
	}
	return ""
}

func flagDir(flags ...string) func(c command) string {
	return func(c command) string {
		for _, f := range flags {
			if v, ok := c.value(f); ok && v != "" && !strings.ContainsAny(v, "$`") {
				if !strings.HasPrefix(v, "/") {
					v = path.Join(c.cwd, v)
				}
				return path.Clean(v)
			}
		}
		return c.cwd
	}
}

var (
	npmDir  = flagDir("--prefix", "-C")
	yarnDir = flagDir("--cwd")
)

func pipInstallsOnly(c command) bool {
	switch {
	case c.prog == "pip" && subIs(c, "install"):
		return true
	case c.prog == "python" && len(c.args) >= 3 && c.args[0] == "-m" && c.args[1] == "pip":
		s := command{args: c.args[2:]}.sub(1)
		return len(s) == 1 && s[0] == "install"
	}
	return false
}

// localPaths reports whether a pip (or uv pip) install installs a folder
// or file of the build context other than its own folder ("/app/pkg",
// "./libs/x", "-e ../y", "dist/x.whl"): such an install needs that code,
// and which files decide what it installs is not known.
func localPaths(c command) bool {
	for i, a := range c.args {
		if strings.HasPrefix(a, "-") || i > 0 && valueOf(c.args[i-1]) {
			continue
		}
		base, _, _ := strings.Cut(a, "[")
		if base == "." || base == "./" || strings.Contains(base, "://") || strings.Contains(base, "@") {
			continue
		}
		if strings.ContainsAny(base, "/") || strings.HasSuffix(base, ".whl") || strings.HasSuffix(base, ".tar.gz") || strings.HasSuffix(base, ".zip") {
			return true
		}
	}
	return false
}

// valueOf reports whether a pip option takes the next argument as its
// value ("-r requirements.txt", "--target /opt").
func valueOf(opt string) bool {
	switch opt {
	case "-r", "--requirement", "-c", "--constraint", "-t", "--target", "--prefix", "--root", "-i", "--index-url",
		"--extra-index-url", "-f", "--find-links", "--cache-dir", "--python", "-p", "--src", "--upgrade-strategy",
		"--only-binary", "--no-binary", "--platform", "--python-version", "--implementation", "--abi", "--progress-bar",
		"--default-index", "--index", "--overrides", "--extra":
		return true
	}
	return false
}

// pipFiles are the files a pip (or uv pip) install reads: its
// requirement files, and the project's own files when it installs a
// folder of the context (".", "./", ".[extra]", "-e .").
func pipFiles(c command) (files []string, project bool) {
	for i, a := range c.args {
		switch {
		case (a == "-r" || a == "--requirement" || a == "-c" || a == "--constraint") && i+1 < len(c.args):
			files = append(files, c.args[i+1])
		case strings.HasPrefix(a, "--requirement="), strings.HasPrefix(a, "--constraint="):
			_, v, _ := strings.Cut(a, "=")
			files = append(files, v)
		case strings.HasPrefix(a, "-r") && len(a) > 2 && !strings.HasPrefix(a, "-r="):
			files = append(files, a[2:])
		}
	}
	if _, _, ok := pipTarget(c); ok {
		project = true
		files = append(files, "pyproject.toml", "setup.py*", "setup.cfg*")
	}
	return files, project
}

// pipTarget is the folder a pip install installs as a project: "." or
// "./" with optional extras; only the build context's own folder counts.
func pipTarget(c command) (target string, extras []string, ok bool) {
	for _, a := range c.args {
		base, ex, _ := strings.Cut(a, "[")
		if base == "." || base == "./" {
			if ex != "" {
				for _, e := range strings.Split(strings.TrimSuffix(ex, "]"), ",") {
					if e = strings.TrimSpace(e); e != "" {
						extras = append(extras, e)
					}
				}
			}
			return a, extras, true
		}
	}
	return "", nil, false
}

// pyprojectDeps writes the dependencies pyproject.toml declares (with
// the extras') to /tmp/requirements.txt; nothing when it has none or
// declares them dynamic.
func pyprojectDeps(extras []string) string {
	list := "p.get('dependencies', [])"
	for _, e := range extras {
		list += " + p.get('optional-dependencies', {}).get('" + e + "', [])"
	}
	return `python -c "import pathlib, tomllib; f = pathlib.Path('pyproject.toml'); ` +
		`p = tomllib.loads(f.read_text()).get('project', {}) if f.exists() else {}; ` +
		`print('\n'.join(` + list + `))" > /tmp/requirements.txt`
}

func pythonAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && (ma > major || ma == major && mi >= minor)
}

// segment is one command of a step's script as the Dockerfile wrote it,
// with the parsed command it stands for.
type segment struct {
	text string
	cmd  command
	ok   bool // parsed into exactly one command
}

// InstallStep is what a RUN step installs and whether its script can be
// cut into commands ("a && b && c", nothing else at the top), which a
// fragment needs to move the installs.
type InstallStep struct {
	Installs []Install
	// Only: the step runs nothing but its installs.
	Only bool
	// Others names the other commands the step runs ("npm run embed").
	Others []string

	segs  []segment
	flags []string
	env   map[string]string
	cwd   string
	which []int // the installer of each segment, -1 for none
}

// Installs reads the dependency installs of a RUN step from its
// operation and its name as BuildKit shows it ("RUN --mount=... npm ci").
// nil when it runs none.
func Installs(op json.RawMessage, name string) *InstallStep {
	defer func() { _ = recover() }()
	s, ok := readStep(op)
	if !ok || len(s.args) == 0 {
		return nil
	}
	text := script(s.args)
	out := &InstallStep{env: s.env, cwd: s.cwd}
	if rest, ok := strings.CutPrefix(name, "RUN "); ok {
		out.flags, _ = runFlags(rest)
	}
	cmds := parse(text, s.env, s.cwd)
	for _, c := range cmds {
		for _, in := range installers {
			files, project, ok := in.match(c)
			if !ok {
				continue
			}
			dir := c.cwd
			if in.dir != nil {
				dir = in.dir(c)
			}
			out.Installs = append(out.Installs, Install{Manager: in.manager, Command: strings.Join(append([]string{c.prog}, c.args...), " "), Dir: dir, Files: files, Project: project})
			break
		}
	}
	if len(out.Installs) == 0 {
		return nil
	}
	// The text of each command, for a fragment: "a && b" only.
	parts, plain := andList(text)
	if plain {
		cwd := s.cwd
		for _, p := range parts {
			cs := parse(p, s.env, cwd)
			sg := segment{text: strings.TrimSpace(p), ok: len(cs) == 1}
			if len(cs) == 1 {
				sg.cmd = cs[0]
			}
			if len(cs) == 0 {
				// cd, export: they set what follows; a fragment does not
				// move such a script.
				sg.ok = false
			}
			out.segs = append(out.segs, sg)
		}
	}
	out.which = make([]int, len(out.segs))
	out.Only = len(out.segs) > 0
	for i, sg := range out.segs {
		out.which[i] = -1
		if !sg.ok {
			out.Only = false
			continue
		}
		for j, in := range installers {
			if _, _, ok := in.match(sg.cmd); ok {
				out.which[i] = j
				break
			}
		}
		if out.which[i] < 0 {
			out.Only = false
			out.Others = append(out.Others, sg.text)
		}
	}
	return out
}

// runFlags splits a RUN instruction's options from its command.
func runFlags(rest string) (flags []string, cmd string) {
	for {
		rest = strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(rest, "--") {
			return flags, rest
		}
		f, r, _ := strings.Cut(rest, " ")
		flags, rest = append(flags, f), r
	}
}

// andList cuts a script at its top-level "&&". plain is false when it
// has anything else at the top (;, ||, |, &, a line break, a subshell, a
// here-document): such a script is not cut.
func andList(text string) (parts []string, plain bool) {
	var (
		quote byte
		depth int
		start int
	)
	text = strings.ReplaceAll(text, "\\\n", " ")
	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case quote != 0:
			if ch == '\\' && quote == '"' {
				i++
			} else if ch == quote {
				quote = 0
			}
		case ch == '\\':
			i++
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '(' || ch == '{':
			depth++
		case ch == ')' || ch == '}':
			depth--
		case depth > 0:
		case ch == '&' && i+1 < len(text) && text[i+1] == '&':
			parts = append(parts, text[start:i])
			i++
			start = i + 1
		case ch == ';' || ch == '|' || ch == '&' || ch == '\n' || ch == '<' && i+1 < len(text) && text[i+1] == '<':
			return nil, false
		case ch == '#' && (i == 0 || text[i-1] == ' '):
			return nil, false
		}
	}
	if quote != 0 || depth != 0 {
		return nil, false
	}
	parts = append(parts, text[start:])
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return nil, false
		}
	}
	return parts, true
}

// Matters reports whether a changed file of the build context is one the
// step's installs read: a change to it reinstalls for good reason. files
// are paths relative to the context; a file whose name a manager reads
// anywhere counts (a workspace's package.json in a sub-folder).
func (s *InstallStep) Matters(file string) bool {
	base := path.Base(file)
	for _, in := range s.Installs {
		for _, x := range installers {
			if x.manager != in.Manager {
				continue
			}
			for _, n := range x.names {
				if base == n {
					return true
				}
			}
		}
		for _, f := range in.Files {
			f = strings.TrimSuffix(f, "*")
			if base == path.Base(f) {
				return true
			}
		}
	}
	return false
}

// Managers lists the managers of the step's installs, once each.
func (s *InstallStep) Managers() []string {
	var out []string
	for _, in := range s.Installs {
		if !contains(out, in.Manager) {
			out = append(out, in.Manager)
		}
	}
	return out
}

// OrderFragment is the Dockerfile text that replaces a COPY of the whole
// context (copy, its instruction as BuildKit shows it) and the RUN step
// right after it that installs dependencies: copy the files the installs
// read, install from them, then copy the rest and run what needs the
// code. parents says whether the build's Dockerfile frontend has COPY
// --parents (1.20 and later), which the JavaScript managers need to copy
// every package.json of a workspace. "" when the step cannot be split
// safely: its script is more than "a && b", a manager has no way to
// install the dependencies alone, or a project folder is not inside what
// the COPY copies.
func (s *InstallStep) OrderFragment(copy string, parents bool) string {
	if s == nil || len(s.segs) == 0 {
		return ""
	}
	_, dest, flags, ok := copyDest(copy)
	if !ok {
		return ""
	}
	if !strings.HasPrefix(dest, "/") {
		dest = path.Join(s.cwd, dest)
	}
	dest = path.Clean(dest)
	x := &orderCtx{env: s.env}
	var before, after, copies []string
	seenDir := map[string]bool{}
	opts := ""
	for _, f := range flags {
		opts += f + " "
	}
	installed := false
	for i, sg := range s.segs {
		if s.which[i] < 0 {
			if !installed {
				// Something the install may need set up first ("poetry
				// config ... && poetry install"): not moved apart.
				return ""
			}
			after = append(after, sg.text)
			continue
		}
		installed = true
		in := installers[s.which[i]]
		files, _, _ := in.match(sg.cmd)
		b, a, ok := in.split(sg.text, sg.cmd, x)
		if !ok {
			return ""
		}
		dir := sg.cmd.cwd
		if in.dir != nil {
			dir = in.dir(sg.cmd)
		}
		rel, inside := relDir(dir, dest)
		if !inside {
			return ""
		}
		if len(in.deep) > 0 && !parents {
			return ""
		}
		if !seenDir[dir] {
			seenDir[dir] = true
			var srcs []string
			for _, f := range in.deep {
				srcs = append(srcs, path.Join(rel, "**", f))
			}
			for _, f := range files {
				if strings.HasPrefix(f, "/") || strings.Contains(f, "..") || strings.ContainsAny(f, "$`") {
					return ""
				}
				srcs = append(srcs, path.Join(rel, f))
			}
			switch {
			case len(srcs) == 0:
				// Named packages only: the install reads no file.
			case len(in.deep) > 0:
				// Every package.json of a workspace, each in its folder.
				copies = append(copies, "COPY --parents "+opts+strings.Join(srcs, " ")+" "+strings.TrimSuffix(dest, "/")+"/")
			default:
				copies = append(copies, "COPY "+opts+strings.Join(srcs, " ")+" "+strings.TrimSuffix(dir, "/")+"/")
			}
		}
		before = append(before, b)
		if a != "" {
			after = append(after, a)
		}
	}
	run := "RUN "
	for _, f := range s.flags {
		run += f + " "
	}
	out := run + strings.Join(before, " \\\n && ") + "\n" + copy
	if len(copies) > 0 {
		out = strings.Join(copies, "\n") + "\n" + out
	}
	if len(after) > 0 {
		out += "\n" + run + strings.Join(after, " \\\n && ")
	}
	return out
}

// copyDest reads a COPY or ADD of the whole build context ("COPY
// --chown=app . /app"): its destination and the options a narrower copy
// keeps (--chown, --chmod, --link). ok false for any other copy.
func copyDest(name string) (verb, dest string, flags []string, ok bool) {
	verb, rest, _ := strings.Cut(name, " ")
	if verb != "COPY" && verb != "ADD" {
		return "", "", nil, false
	}
	var words []string
	for _, w := range strings.Fields(rest) {
		if strings.HasPrefix(w, "--") {
			if strings.HasPrefix(w, "--from") {
				return "", "", nil, false
			}
			for _, keep := range []string{"--chown=", "--chmod=", "--link"} {
				if strings.HasPrefix(w, keep) {
					flags = append(flags, w)
				}
			}
			continue
		}
		words = append(words, w)
	}
	if len(words) != 2 || (words[0] != "." && words[0] != "./") || strings.ContainsAny(words[1], "$`") {
		return "", "", nil, false
	}
	return verb, words[1], flags, true
}

// WholeContext reports whether an instruction copies the whole build
// context ("COPY . .", "ADD --chown=x ./ /app").
func WholeContext(name string) bool {
	_, _, _, ok := copyDest(name)
	return ok
}

// relDir is dir relative to base, when dir is base or inside it.
func relDir(dir, base string) (string, bool) {
	dir, base = path.Clean(dir), path.Clean(base)
	if dir == base {
		return ".", true
	}
	if r, ok := strings.CutPrefix(dir, strings.TrimSuffix(base, "/")+"/"); ok {
		return r, true
	}
	return "", false
}

// replaceWord replaces the first whitespace-separated word of cmd that
// is w (quoted or not) with by.
func replaceWord(cmd, w, by string) string {
	for _, t := range tokens(cmd) {
		x := strings.Trim(cmd[t[0]:t[1]], `"'`)
		if x == w {
			return cmd[:t[0]] + by + cmd[t[1]:]
		}
	}
	return cmd
}

// insertAfterWord puts an option after the first word that is after.
func insertAfterWord(cmd, after, add string) string {
	for _, t := range tokens(cmd) {
		if cmd[t[0]:t[1]] == after {
			return cmd[:t[1]] + " " + add + cmd[t[1]:]
		}
	}
	return cmd
}
