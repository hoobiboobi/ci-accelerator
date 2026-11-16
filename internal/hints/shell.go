package hints

import (
	"path"
	"regexp"
	"strings"
)

// command is one simple command of a RUN step's script: its program (the
// base name, wrappers removed), its arguments, and the environment and
// working directory it runs with, as far as the script itself sets them.
type command struct {
	prog string
	args []string
	env  map[string]string
	cwd  string
}

// arg reports whether a command has an argument, or one starting with
// prefix when prefix ends with '='.
func (c command) arg(a string) bool {
	for _, x := range c.args {
		if x == a || strings.HasSuffix(a, "=") && strings.HasPrefix(x, a) {
			return true
		}
	}
	return false
}

// value returns a flag's value: "--cache x", "--cache=x" or, for flags
// like "-Dmaven.repo.local=", the text after the prefix.
func (c command) value(flag string) (string, bool) {
	for i, x := range c.args {
		switch {
		case strings.HasSuffix(flag, "="):
			if strings.HasPrefix(x, flag) {
				return strings.TrimPrefix(x, flag), true
			}
		case x == flag && i+1 < len(c.args):
			return c.args[i+1], true
		case strings.HasPrefix(x, flag+"="):
			return strings.TrimPrefix(x, flag+"="), true
		}
	}
	return "", false
}

// sub returns the command's first arguments that are not options, at most
// n: "install" for "npm --prefer-offline install x".
func (c command) sub(n int) []string {
	var out []string
	for _, x := range c.args {
		if len(out) == n {
			break
		}
		if strings.HasPrefix(x, "-") {
			continue
		}
		out = append(out, x)
	}
	return out
}

// script is what a step runs: the text after "-c" when the step runs a
// shell (shell-form RUN, also under SHELL ["/bin/bash", "-o", "pipefail",
// "-c"]), the arguments joined for an exec form.
func script(args []string) string {
	if len(args) >= 3 {
		switch path.Base(args[0]) {
		case "sh", "bash", "ash", "dash", "zsh", "busybox":
			for i := 1; i+1 < len(args); i++ {
				if a := args[i]; strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
					return args[i+1]
				}
			}
		}
	}
	// A SHELL of the image's own (micromamba's _dockerfile_shell.sh, a
	// login wrapper) gets the command as its last argument.
	if last := args[len(args)-1]; len(args) >= 2 && strings.ContainsAny(last, " \t\n;&|") {
		return last
	}
	return strings.Join(args, " ")
}

// maxScript bounds what is read of a step's script: a RUN step's text is
// the client's to send.
const maxScript = 64 << 10

// maxDepth bounds "sh -c" nested in a script.
const maxDepth = 4

// maxValue bounds a variable's value after expansion: "A=$A$A" repeated
// doubles it every line, and a script of a few hundred bytes would
// otherwise take gigabytes. A longer value stays unexpanded, so a path
// holding it is not used.
const maxValue = 4 << 10

// maxSpend bounds what reading one script may allocate for the
// environments of its commands and their expanded words (an environment
// entry counts as 64 bytes): a script with thousands of assignments and
// thousands of commands would otherwise copy every variable into every
// command. Past it, the rest of the script is not read, which errs
// towards no hint.
const maxSpend = 8 << 20

// reader keeps what reading one script has spent.
type reader struct{ spent int }

func (r *reader) expand(s string, env map[string]string) string {
	v := expand(s, env)
	r.spent += len(v)
	return v
}

func (r *reader) merge(a, b map[string]string) map[string]string {
	r.spent += 64 * (len(a) + len(b))
	return merge(a, b)
}

// parse splits a script into simple commands. It is a reader of common
// shell, not a shell: quotes, escapes, comments, &&, ||, ;, |, &, line
// breaks, subshell parentheses and braces, leading assignments, export,
// cd and the usual wrappers (sudo, env, time, nice, timeout, nohup, exec,
// command, xargs, sh -c "..."). Anything it does not understand ends up as
// words that match no rule, which errs towards no hint.
func parse(text string, env map[string]string, cwd string) []command {
	var out []command
	r := &reader{}
	r.parseInto(&out, text, env, cwd, 0)
	return out
}

func (r *reader) parseInto(out *[]command, text string, env map[string]string, cwd string, depth int) {
	if len(text) > maxScript {
		text = text[:maxScript]
	}
	if cwd == "" {
		cwd = "/"
	}
	shellEnv := r.merge(env, nil)
	for _, words := range split(text) {
		if r.spent > maxSpend {
			return
		}
		inline := map[string]string{}
		i := 0
		for ; i < len(words) && assignment(words[i]); i++ {
			k, v, _ := strings.Cut(words[i], "=")
			inline[k] = r.expand(v, shellEnv)
		}
		words = r.unwrap(words[i:], inline, shellEnv)
		if len(words) == 0 {
			// "A=b" alone sets a shell variable for what follows.
			for k, v := range inline {
				shellEnv[k] = v
			}
			continue
		}
		prog := path.Base(words[0])
		switch prog {
		case "export":
			for _, w := range words[1:] {
				if k, v, ok := strings.Cut(w, "="); ok && identifier.MatchString(k) {
					shellEnv[k] = r.expand(v, shellEnv)
				}
			}
			continue
		case "cd":
			if len(words) > 1 {
				d := r.expand(words[1], shellEnv)
				if !strings.HasPrefix(d, "/") {
					d = path.Join(cwd, d)
				}
				cwd = path.Clean(d)
			}
			continue
		case "sh", "bash", "ash", "dash", "zsh":
			if depth < maxDepth {
				for j := 1; j+1 < len(words); j++ {
					if strings.HasPrefix(words[j], "-") && strings.Contains(words[j], "c") && !strings.HasPrefix(words[j], "--") {
						r.parseInto(out, words[j+1], r.merge(shellEnv, inline), cwd, depth+1)
						break
					}
				}
			}
			continue
		}
		c := command{prog: normalize(prog), args: words[1:], env: r.merge(shellEnv, inline), cwd: cwd}
		for i, a := range c.args {
			c.args[i] = r.expand(a, c.env)
		}
		*out = append(*out, c)
	}
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func assignment(w string) bool {
	k, _, ok := strings.Cut(w, "=")
	return ok && identifier.MatchString(k)
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// normalize names a program without its version: pip3.12 -> pip,
// python3 -> python, ./gradlew -> gradlew.
func normalize(prog string) string {
	for _, p := range []string{"pip", "python", "ruby", "gem", "bundle", "php", "node"} {
		if prog == p || strings.HasPrefix(prog, p) && strings.Trim(prog[len(p):], "0123456789.") == "" {
			return p
		}
	}
	return prog
}

// unwrap removes the wrappers in front of a command, adding the
// assignments "env" takes to inline.
func (r *reader) unwrap(words []string, inline, shellEnv map[string]string) []string {
	for len(words) > 0 {
		switch path.Base(words[0]) {
		case "sudo", "doas", "exec", "command", "builtin", "nohup", "time", "!", "if", "then", "else", "elif", "do", "while", "until", "stdbuf", "chronic", "eatmydata":
			words = skipOptions(words[1:])
		case "env":
			words = skipOptions(words[1:])
			for len(words) > 0 && assignment(words[0]) {
				k, v, _ := strings.Cut(words[0], "=")
				inline[k] = r.expand(v, r.merge(shellEnv, inline))
				words = words[1:]
			}
		case "nice", "ionice":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if (words[0] == "-n" || words[0] == "-c") && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
		case "timeout":
			words = skipOptions(words[1:])
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		case "xargs":
			words = skipOptions(words[1:])
		case "npx", "bunx", "corepack":
			// "npx pnpm@9 install", "corepack yarn install": the package
			// manager they start is the command.
			rest := skipOptions(words[1:])
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return words
			}
			name, _, _ := strings.Cut(rest[0], "@")
			switch name {
			case "pnpm", "yarn", "npm", "bun", "next", "turbo", "nx":
				rest[0] = name
				words = rest
			default:
				return words
			}
		default:
			return words
		}
	}
	return words
}

// skipOptions drops leading options ("-u", "--preserve-env", "-E"); an
// option's separate value is not told apart, which only matters for
// wrappers that take one ("sudo -u user": the user is skipped as the
// option's value).
func skipOptions(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
		if words[0] == "-u" || words[0] == "-g" || words[0] == "-s" || words[0] == "-k" || words[0] == "-I" || words[0] == "-n" || words[0] == "-P" || words[0] == "-L" {
			if len(words) > 1 {
				words = words[1:]
			}
		}
		words = words[1:]
	}
	return words
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-=][^}]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expand replaces $VAR and ${VAR} (with :- / - defaults) it knows; a
// reference it does not know stays, so a path holding one is not used.
// A result longer than maxValue is not made: s comes back as it is.
func expand(s string, env map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	size, long := len(s), false
	out := varRef.ReplaceAllStringFunc(s, func(m string) string {
		if long {
			return m
		}
		sub := varRef.FindStringSubmatch(m)
		name := sub[1] + sub[3]
		v, ok := env[name]
		switch {
		case ok && (v != "" || !strings.HasPrefix(sub[2], ":")):
		case strings.HasPrefix(sub[2], ":-") || strings.HasPrefix(sub[2], "-") || strings.HasPrefix(sub[2], ":=") || strings.HasPrefix(sub[2], "="):
			v = strings.TrimLeft(sub[2], ":-=")
		default:
			return m
		}
		if size += len(v) - len(m); size > maxValue {
			long = true
			return m
		}
		return v
	})
	if long {
		return s
	}
	return out
}

// split cuts a script into the words of its simple commands.
func split(text string) [][]string {
	var (
		out    [][]string
		words  []string
		word   strings.Builder
		inWord bool
		quote  byte
	)
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			switch {
			case ch == quote:
				quote = 0
			case ch == '\\' && quote == '"' && i+1 < len(text):
				i++
				if text[i] != '\n' {
					word.WriteByte(text[i])
				}
			default:
				word.WriteByte(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote, inWord = ch, true
		case '\\':
			if i+1 < len(text) {
				i++
				if text[i] != '\n' {
					word.WriteByte(text[i])
					inWord = true
				}
			}
		case ' ', '\t', '\r':
			endWord()
		case '\n', ';', '&', '|', '(', ')':
			endCommand()
		case '{', '}':
			if !inWord {
				endCommand()
			} else {
				word.WriteByte(ch)
			}
		case '#':
			if !inWord {
				for i < len(text) && text[i] != '\n' {
					i++
				}
				endCommand()
			} else {
				word.WriteByte(ch)
			}
		case '`':
			// A command substitution's text is left out.
			j := strings.IndexByte(text[i+1:], '`')
			if j < 0 {
				i = len(text)
			} else {
				i += j + 1
			}
			inWord = true
		case '$':
			if i+1 < len(text) && text[i+1] == '(' {
				// $( ... ): skipped to its closing parenthesis.
				d := 0
				j := i + 1
				for ; j < len(text); j++ {
					if text[j] == '(' {
						d++
					} else if text[j] == ')' {
						d--
						if d == 0 {
							break
						}
					}
				}
				i, inWord = j, true
				continue
			}
			word.WriteByte(ch)
			inWord = true
		default:
			word.WriteByte(ch)
			inWord = true
		}
	}
	endCommand()
	return out
}
