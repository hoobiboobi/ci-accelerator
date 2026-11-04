// Package hints finds image build steps that run a package manager without
// a cache mount on its cache, or with one that cannot work, and says what
// to put in the Dockerfile. It reads a step's LLB operation as BuildKit
// recorded it (command, environment, user, working directory, mounts),
// never the Dockerfile's text, and it never guesses a path: when the
// operation does not say where a manager keeps its cache, the hint says
// how to find out instead.
package hints

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Kind is what is wrong with a step's cache.
type Kind string

const (
	// Missing: no cache mount covers the manager's cache.
	Missing Kind = "missing"
	// Disabled: a cache mount is there, but a flag, a setting or a
	// command in the build switches the cache off or empties it.
	Disabled Kind = "disabled"
	// WrongPath: a cache mount for the manager is there, but not where
	// the manager writes (another user's home, a variable moving it).
	WrongPath Kind = "wrong-path"
	// ImageNeeds: a cache mount is on something the image needs (what
	// the manager installs, or build output not copied out of it): a
	// cache mount is not part of the image.
	ImageNeeds Kind = "image-needs"
	// NotLocked: a system package manager shares its cache with other
	// builds without sharing=locked, and two at once fail on its lock.
	NotLocked Kind = "not-locked"
	// NotWritable: the step runs as a user other than root, and the cache
	// mount has no owner or mode: BuildKit makes it root's, 0755, and the
	// manager cannot write to it.
	NotWritable Kind = "not-writable"
)

// Hint is what one package manager of a step needs.
type Hint struct {
	Manager string `json:"manager"`
	// Problems found; a hint without a Missing problem is about a cache
	// mount that is there but cannot work.
	Problems []Problem `json:"problems"`
	// Caches are the manager's cache folders this step needs, as the
	// step's environment places them; a folder whose Path is "" could
	// not be placed (see PathCommand).
	Caches []Cache `json:"caches"`
	// PathCommand prints the manager's cache folder in the image.
	PathCommand string `json:"pathCommand,omitempty"`
	// Before are instructions to add before the step, Drop words to take
	// out of its command, Insert options to put after the manager's name.
	Before []string `json:"before,omitempty"`
	Drop   []string `json:"drop,omitempty"`
	Insert []Insert `json:"insert,omitempty"`
	// Caveats that apply to this manager's cache mount.
	Caveats []string `json:"caveats,omitempty"`
	// Owner is the mount option that lets the step's user write to a
	// cache mount ("uid=1000,gid=1000", or "mode=0777" for a user known
	// only by name); "" when the step runs as root.
	Owner string `json:"owner,omitempty"`
}

// Problem is one thing wrong with a step's cache.
type Problem struct {
	Kind Kind `json:"kind"`
	// Mount is the cache mount's target the problem is about (not for
	// Missing).
	Mount string `json:"mount,omitempty"`
	// Setting is what switches the cache off (Disabled): a flag, a
	// variable, a command, or a setting the build leaves in place.
	Setting string `json:"setting,omitempty"`
	// Cache is the label of the cache folder (Missing, WrongPath).
	Cache string `json:"cache,omitempty"`
}

// Cache is one cache folder of a manager.
type Cache struct {
	Label  string `json:"label"`
	Path   string `json:"path,omitempty"`
	Locked bool   `json:"locked,omitempty"`
	// ID is the cache mount id to suggest when the path depends on the
	// step (a project folder), so that every project's builds share it.
	ID string `json:"id,omitempty"`
}

// Insert puts an option after a manager's name in the command.
type Insert struct {
	After string `json:"after"`
	Add   string `json:"add"`
}

// HasMissing reports whether a hint is about a missing cache mount.
func (h Hint) HasMissing() bool {
	for _, p := range h.Problems {
		if p.Kind == Missing {
			return true
		}
	}
	return false
}

// Broken reports whether a hint is about a cache mount that is there but
// cannot work.
func (h Hint) Broken() bool {
	for _, p := range h.Problems {
		if p.Kind != Missing {
			return true
		}
	}
	return false
}

// step is the part of an exec operation hints read.
type step struct {
	args   []string
	env    map[string]string
	user   string
	cwd    string
	mounts []mount
}

type mount struct {
	dest    string
	cache   bool
	locked  bool
	id      string
	sharing string
	// owned: the mount is made from a folder the frontend creates with
	// an owner or mode (uid, gid or mode set), not root's 0755 default. A
	// mount with from= has an input too; its owner is not known, and it
	// counts as owned (no hint rather than a wrong one).
	owned bool
}

type rawOp struct {
	Op struct {
		Exec *struct {
			Meta struct {
				Args []string `json:"args"`
				Env  []string `json:"env"`
				Cwd  string   `json:"cwd"`
				User string   `json:"user"`
			} `json:"meta"`
			Mounts []struct {
				Dest      string `json:"dest"`
				MountType int    `json:"mountType"`
				Input     *int   `json:"input"`
				CacheOpt  *struct {
					ID      string          `json:"ID"`
					Sharing json.RawMessage `json:"sharing"`
				} `json:"cacheOpt"`
			} `json:"mounts"`
		} `json:"exec"`
	} `json:"Op"`
}

// mountType 3 is a cache mount; sharing 2 is "locked" (BuildKit's
// pb.MountType and pb.CacheSharingOpt).
const (
	cacheMount    = 3
	sharingLocked = 2
)

func readStep(op json.RawMessage) (step, bool) {
	var r rawOp
	if err := json.Unmarshal(op, &r); err != nil || r.Op.Exec == nil {
		return step{}, false
	}
	e := r.Op.Exec
	s := step{args: e.Meta.Args, env: map[string]string{}, user: e.Meta.User, cwd: e.Meta.Cwd}
	for _, kv := range e.Meta.Env {
		k, v, _ := strings.Cut(kv, "=")
		s.env[k] = v
	}
	for _, m := range e.Mounts {
		x := mount{dest: path.Clean("/" + m.Dest), cache: m.MountType == cacheMount, owned: m.Input != nil && *m.Input >= 0}
		if m.CacheOpt != nil {
			x.id = m.CacheOpt.ID
			x.sharing = strings.Trim(string(m.CacheOpt.Sharing), `"`)
			n, err := strconv.Atoi(x.sharing)
			x.locked = err == nil && n == sharingLocked || strings.EqualFold(x.sharing, "LOCKED")
		}
		s.mounts = append(s.mounts, x)
	}
	return s, true
}

// Analyze returns the hints for a step's operation; ancestors are the
// operations the step builds on (any order), read for what earlier steps
// set up (apt's docker-clean removed, dnf's keepcache). A step that is
// not a command, or runs no known package manager, has none.
func Analyze(op json.RawMessage, ancestors []json.RawMessage) (out []Hint) {
	// The operation is the build client's to send: a reading this package
	// gets wrong costs the step its hint, never the page.
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	s, ok := readStep(op)
	if !ok || len(s.args) == 0 {
		return nil
	}
	text := script(s.args)
	ctx := &context{step: s, text: text, home: home(s.env, s.user)}
	for _, a := range ancestors {
		if as, ok := readStep(a); ok {
			ctx.earlier = append(ctx.earlier, script(as.args))
		}
	}
	ctx.commands = parse(text, s.env, s.cwd)
	for _, m := range managers {
		var uses []command
		for _, c := range ctx.commands {
			if m.match(c) {
				uses = append(uses, c)
			}
		}
		if len(uses) == 0 {
			continue
		}
		if h, ok := ctx.check(m, uses); ok {
			out = append(out, h)
		}
	}
	return out
}

// home is the step's home folder: $HOME, or root's when the step runs as
// root; "" when it runs as another user (its home is in the image's
// /etc/passwd, which the operation does not show).
func home(env map[string]string, user string) string {
	if h := env["HOME"]; strings.HasPrefix(h, "/") {
		return path.Clean(h)
	}
	u, _, _ := strings.Cut(user, ":")
	if u == "" || u == "root" || u == "0" {
		return "/root"
	}
	return ""
}

type context struct {
	step     step
	text     string
	earlier  []string
	home     string
	commands []command
}

// mentions reports whether this step's script or an earlier step's has
// any of the words.
func (x *context) mentions(words ...string) bool {
	for _, t := range append([]string{x.text}, x.earlier...) {
		for _, w := range words {
			if strings.Contains(t, w) {
				return true
			}
		}
	}
	return false
}

// runs reports whether the step runs a command that matches.
func (x *context) runs(f func(command) bool) bool {
	for _, c := range x.commands {
		if f(c) {
			return true
		}
	}
	return false
}

func (x *context) cacheMounts() []mount {
	var out []mount
	for _, m := range x.step.mounts {
		if m.cache {
			out = append(out, m)
		}
	}
	return out
}

// owner is the mount option that lets a step's user write to a cache
// mount: its uid (and gid) when the step names them by number; mode 0777
// for a user named by name, whose number is in the image's /etc/passwd;
// "" for root.
func owner(user string) string {
	u, g, _ := strings.Cut(user, ":")
	if u == "" || u == "root" || u == "0" {
		return ""
	}
	if _, err := strconv.Atoi(u); err != nil {
		return "mode=0777"
	}
	if _, err := strconv.Atoi(g); err == nil {
		return "uid=" + u + ",gid=" + g
	}
	return "uid=" + u
}

// othersHome reports whether a folder is in root's home while the step
// runs as another user: whatever that user's home is, a manager running
// as it does not write there.
func (x *context) othersHome(dir string) bool {
	return x.home == "" && within(dir, "/root")
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// covers reports whether a mount at dest holds the folder p: dest is p,
// one of its parents, or a folder inside it (a mount on apt's archives
// folder covers what apt keeps of /var/cache/apt).
func covers(dest, p string) bool {
	return dest != "/" && (within(p, dest) || within(dest, p))
}

// check works out one manager's hint for the commands of a step that run
// it.
func (x *context) check(m *manager, uses []command) (Hint, bool) {
	h := Hint{Manager: m.name, PathCommand: m.pathCommand, Owner: owner(x.step.user)}
	mounts := x.cacheMounts()
	type placed struct {
		dir  cacheDir
		path string // "" unknown
	}
	var dirs []placed
	for _, d := range m.caches {
		if d.need != nil && !needed(d, uses) {
			continue
		}
		p := x.place(d, uses)
		dirs = append(dirs, placed{d, p})
	}
	if len(dirs) == 0 {
		return Hint{}, false
	}
	anyCovered := false
	for _, d := range dirs {
		c := Cache{Label: d.dir.label, Path: d.path, Locked: m.locked}
		if d.dir.cwd != "" && d.path != "" {
			c.ID = d.dir.id
		}
		h.Caches = append(h.Caches, c)
		covered := false
		var near *mount
		for i, mt := range mounts {
			holds := d.path != "" && covers(mt.dest, d.path) || d.path == "" && m.marks(d.dir, mt.dest) && !x.othersHome(mt.dest)
			if d.dir.also != nil {
				for _, p := range d.dir.also(x) {
					holds = holds || covers(mt.dest, p)
				}
			}
			switch {
			case holds:
				// Not known where it writes: a mount that looks like the
				// manager's cache is taken for it.
				covered = true
				if m.locked && !mt.locked {
					h.Problems = append(h.Problems, Problem{Kind: NotLocked, Mount: mt.dest})
				}
				if h.Owner != "" && !mt.owned {
					h.Problems = append(h.Problems, Problem{Kind: NotWritable, Mount: mt.dest})
				}
			case m.marks(d.dir, mt.dest):
				near = &mounts[i]
			}
		}
		switch {
		case covered:
			anyCovered = true
		case near != nil:
			h.Problems = append(h.Problems, Problem{Kind: WrongPath, Mount: near.dest, Cache: d.dir.label})
		default:
			h.Problems = append(h.Problems, Problem{Kind: Missing, Cache: d.dir.label})
		}
	}
	// What switches the cache off matters once a cache mount is there;
	// without one, the fix drops it with the mount added.
	if off := x.disabled(m, uses); off != "" {
		if anyCovered {
			h.Problems = append(h.Problems, Problem{Kind: Disabled, Setting: off})
		}
		if m.dropFlag(off) {
			h.Drop = append(h.Drop, off)
		}
	}
	if m.extra != nil {
		m.extra(x, uses, &h, anyCovered)
	}
	for _, mt := range mounts {
		if why := x.imageNeeds(m, uses, mt.dest); why != "" {
			h.Problems = append(h.Problems, Problem{Kind: ImageNeeds, Mount: mt.dest, Setting: why})
		}
	}
	h.Problems = dedupe(h.Problems)
	h.Caveats = append(h.Caveats, m.caveats...)
	switch {
	case h.Owner == "mode=0777":
		h.Caveats = append(h.Caveats, "The step runs as "+x.step.user+", not root: a cache mount is root's (mode 0755) unless it says otherwise, so the fragment opens it with mode=0777; uid= and gid= with that user's numbers (id -u and id -g in the image) are the stricter choice.")
	case h.Owner != "":
		h.Caveats = append(h.Caveats, "The step runs as user "+x.step.user+", not root: a cache mount is root's (mode 0755) unless it says otherwise, so the fragment gives it that user's "+strings.ReplaceAll(h.Owner, ",", " and ")+".")
	}
	if len(h.Problems) == 0 {
		return Hint{}, false
	}
	return h, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func dedupe(ps []Problem) []Problem {
	seen := map[Problem]bool{}
	var out []Problem
	for _, p := range ps {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func needed(d cacheDir, uses []command) bool {
	for _, c := range uses {
		if d.need(c) {
			return true
		}
	}
	return false
}

// place works out where a cache folder is for a step: a flag on the
// command, then the variables (the command's own first), then the
// default under the home or XDG folder, or a fixed one. "" when it
// depends on something the operation does not say.
func (x *context) place(d cacheDir, uses []command) string {
	c := uses[0]
	for _, f := range d.flags {
		if v, ok := c.value(f); ok {
			return x.abs(v, c.cwd)
		}
	}
	for _, v := range d.env {
		if val, ok := c.env[v.name]; ok && val != "" {
			if v.list {
				val, _, _ = strings.Cut(val, ":")
			}
			return x.abs(val+v.suffix, c.cwd)
		}
	}
	switch {
	case d.abs != "":
		return d.abs
	case d.cwd != "":
		return path.Join(c.cwd, d.cwd)
	case d.xdg != "":
		if v := c.env["XDG_CACHE_HOME"]; strings.HasPrefix(v, "/") {
			return path.Join(v, d.xdg)
		}
		if x.home == "" && c.env["HOME"] == "" {
			return ""
		}
		return path.Join(firstNonEmpty(absOnly(c.env["HOME"]), x.home), ".cache", d.xdg)
	case d.home != "":
		h := firstNonEmpty(absOnly(c.env["HOME"]), x.home)
		if h == "" {
			return ""
		}
		return path.Join(h, d.home)
	}
	return ""
}

func absOnly(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return ""
}

// abs makes a path from a flag or variable absolute; "" when it still
// holds a variable the step does not set, or is not a plain path.
func (x *context) abs(p, cwd string) string {
	if p == "" || strings.ContainsAny(p, "$`*?") {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if x.home == "" {
			return ""
		}
		p = path.Join(x.home, p[2:])
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(cwd, p)
	}
	return path.Clean(p)
}

// disabled returns what switches the manager's cache off in this step, or
// "".
func (x *context) disabled(m *manager, uses []command) string {
	for _, c := range uses {
		for _, f := range m.offFlags {
			if c.arg(f) {
				return f
			}
		}
		for _, v := range m.offEnv {
			if val, ok := c.env[v]; ok && truthy(val) {
				return v
			}
		}
	}
	for _, c := range x.commands {
		if m.wipes != nil {
			if w := m.wipes(c); w != "" {
				return w
			}
		}
	}
	return ""
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// imageNeeds says why a cache mount holds something the image needs, or
// "".
func (x *context) imageNeeds(m *manager, uses []command, dest string) string {
	for _, c := range uses {
		for _, t := range m.targets {
			if p := t(x, c); p != "" && within(p, dest) {
				return "installs into " + p
			}
		}
	}
	for _, o := range m.outputs {
		for _, c := range uses {
			p := o(x, c)
			if p != "" && (dest == p || within(p, dest)) && !x.copiesOut(dest) {
				return "build output in " + p + " is not copied out"
			}
		}
	}
	return ""
}

// copiesOut reports whether the step copies something out of a folder
// (cp, mv, rsync, install, tar), which is how build output in a cache
// mount reaches the image.
func (x *context) copiesOut(dir string) bool {
	for _, c := range x.commands {
		switch c.prog {
		case "cp", "mv", "rsync", "install", "tar", "cpio":
			var operands []string
			for _, a := range c.args {
				if !strings.HasPrefix(a, "-") {
					operands = append(operands, a)
				}
			}
			// cp, mv, rsync and install copy into their last operand: only
			// what comes before it is read from the folder.
			if c.prog != "tar" && c.prog != "cpio" && len(operands) > 1 {
				operands = operands[:len(operands)-1]
			}
			for _, a := range operands {
				p := a
				if !strings.HasPrefix(p, "/") {
					p = path.Join(c.cwd, p)
				}
				if within(path.Clean(p), dir) {
					return true
				}
			}
		}
	}
	return false
}
