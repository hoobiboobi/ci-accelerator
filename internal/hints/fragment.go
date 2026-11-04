package hints

import (
	"path"
	"strings"
)

// Fragment is the Dockerfile text that fixes a step: instructions to add
// before it, then the step's RUN with the cache mounts it needs. name is
// the step's name as BuildKit shows it ("RUN --mount=... npm ci"), with
// secrets already masked; the step's other options (secret mounts,
// network) and the mounts that are right stay as they are. "" when the
// step is not a RUN.
func Fragment(name string, hs []Hint) string {
	rest, ok := strings.CutPrefix(name, "RUN ")
	if !ok || len(hs) == 0 {
		return ""
	}
	var flags []string
	for {
		rest = strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(rest, "--") {
			break
		}
		f, r, _ := strings.Cut(rest, " ")
		flags, rest = append(flags, f), r
	}
	drop, lock, own := map[string]bool{}, map[string]bool{}, map[string]string{}
	var before []string
	for _, h := range hs {
		for _, p := range h.Problems {
			switch p.Kind {
			case WrongPath, ImageNeeds:
				drop[p.Mount] = true
			case NotLocked:
				lock[p.Mount] = true
			case NotWritable:
				own[p.Mount] = h.Owner
			}
		}
		for _, b := range h.Before {
			if !contains(before, b) {
				before = append(before, b)
			}
		}
	}
	var mounts, others []string
	var kept []string // targets of cache mounts that stay
	for _, f := range flags {
		target, cache := cacheTarget(f)
		switch {
		case !cache:
			others = append(others, f)
		case drop[target]:
		case lock[target] || own[target] != "":
			if lock[target] {
				f = locked(f)
			}
			if own[target] != "" {
				f += "," + own[target]
			}
			mounts, kept = append(mounts, f), append(kept, target)
		default:
			mounts, kept = append(mounts, f), append(kept, target)
		}
	}
	for _, h := range hs {
		for _, c := range h.Caches {
			if c.Path != "" && coveredBy(kept, c.Path) {
				continue
			}
			target := c.Path
			if target == "" {
				target = "<" + h.Manager + " " + c.Label + " folder>"
			}
			m := "--mount=type=cache,target=" + target
			if c.ID != "" {
				m += ",id=" + c.ID
			}
			if c.Locked {
				m += ",sharing=locked"
			}
			if h.Owner != "" {
				m += "," + h.Owner
			}
			if !contains(mounts, m) {
				mounts = append(mounts, m)
			}
			kept = append(kept, c.Path)
		}
	}
	cmd := rest
	for _, h := range hs {
		for _, d := range h.Drop {
			cmd = dropWord(cmd, d)
		}
		for _, in := range h.Insert {
			cmd = insertAfter(cmd, in.After, in.Add)
		}
	}
	parts := append(others, mounts...)
	var b strings.Builder
	for _, l := range before {
		b.WriteString(l + "\n")
	}
	b.WriteString("RUN")
	if len(parts) <= 1 && len("RUN "+strings.Join(parts, " ")+" "+cmd) <= 80 {
		for _, p := range parts {
			b.WriteString(" " + p)
		}
		b.WriteString(" " + cmd)
		return b.String()
	}
	for _, p := range parts {
		b.WriteString(" " + p + " \\\n   ")
	}
	b.WriteString(" " + cmd)
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func coveredBy(targets []string, p string) bool {
	for _, t := range targets {
		if t != "" && covers(t, p) {
			return true
		}
	}
	return false
}

// cacheTarget reads a RUN option: whether it is a cache mount, and its
// target.
func cacheTarget(flag string) (string, bool) {
	opts, ok := strings.CutPrefix(flag, "--mount=")
	if !ok {
		return "", false
	}
	cache, target := false, ""
	for _, kv := range strings.Split(opts, ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "type":
			cache = v == "cache"
		case "target", "dst", "destination":
			target = cleanPath(v)
		}
	}
	return target, cache
}

// cleanPath reads a target the way the operation records it (readStep):
// cleaned and absolute.
func cleanPath(p string) string {
	if p == "" {
		return p
	}
	return path.Clean("/" + p)
}

// locked sets sharing=locked on a cache mount option.
func locked(flag string) string {
	opts := strings.Split(strings.TrimPrefix(flag, "--mount="), ",")
	out := opts[:0]
	for _, kv := range opts {
		if k, _, _ := strings.Cut(kv, "="); k != "sharing" {
			out = append(out, kv)
		}
	}
	return "--mount=" + strings.Join(append(out, "sharing=locked"), ",")
}

// dropWord removes a word (a flag, or VAR=value for a variable name) from
// a command.
func dropWord(cmd, w string) string {
	var b strings.Builder
	last := 0
	for _, t := range tokens(cmd) {
		x := cmd[t[0]:t[1]]
		if x == w || strings.HasPrefix(x, w+"=") {
			// The word and the space after it.
			b.WriteString(cmd[last:t[0]])
			end := t[1]
			for end < len(cmd) && cmd[end] == ' ' {
				end++
			}
			last = end
		}
	}
	b.WriteString(cmd[last:])
	return b.String()
}

// tokens are the start and end of every run of non-space characters.
func tokens(s string) [][2]int {
	var out [][2]int
	start := -1
	for i := 0; i <= len(s); i++ {
		space := i == len(s) || s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r'
		switch {
		case !space && start < 0:
			start = i
		case space && start >= 0:
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	return out
}

// insertAfter puts an option after the first word that is prog.
func insertAfter(cmd, prog, add string) string {
	if strings.Contains(cmd, add) {
		return cmd
	}
	for _, t := range tokens(cmd) {
		if x := cmd[t[0]:t[1]]; x == prog || strings.HasSuffix(x, "/"+prog) {
			return cmd[:t[1]] + " " + add + cmd[t[1]:]
		}
	}
	return cmd
}
