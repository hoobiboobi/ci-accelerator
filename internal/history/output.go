package history

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MaxStepOutput bounds the output kept for one step. BuildKit itself clips a
// step's log at 2 MiB by default. Errors are usually at the end, so most of
// the budget goes to the tail.
const MaxStepOutput = 64 << 10

const outputHead = 8 << 10

// Masked replaces a secret in kept output and in reasons.
const Masked = "[masked]"

// secretPatterns are token formats that are secret wherever they appear.
// The prefixed ones need no word boundary before them: a token may follow
// a letter, as after a printf's literal \n, and prefix and length are
// distinctive enough on their own.
var secretPatterns = []secretPattern{
	// GitLab tokens, by their documented prefixes, and runner registration
	// tokens.
	pat(`(?:glpat|gloas|gldt|glrtr|glrt|glcbt|glptt|glft|glimt|glagent|glwt|glsoat|glffct)-[0-9A-Za-z_.\-]{16,}`, "glpat", "gloas", "gldt", "glrt", "glcbt", "glptt", "glft", "glimt", "glagent", "glwt", "glsoat", "glffct"),
	pat(`GR1348941[0-9A-Za-z_\-]{20,}`, "gr1348941"),
	pat(`_gitlab_session=[0-9A-Za-z]+`, "_gitlab_session="),
	// GitHub tokens.
	pat(`gh[pousr]_[0-9A-Za-z]{36,}`, "ghp_", "gho_", "ghu_", "ghs_", "ghr_"),
	pat(`github_pat_[0-9A-Za-z_]{22,}`, "github_pat_"),
	// Slack tokens and webhook addresses.
	pat(`xox[abposr]-[0-9A-Za-z\-]{10,}`, "xox"),
	pat(`https://hooks\.slack\.com/services/[0-9A-Za-z/_\-]+`, "hooks.slack.com"),
	// npm, PyPI, Stripe, Google API and Vault tokens.
	pat(`npm_[0-9A-Za-z]{36,}`, "npm_"),
	pat(`pypi-AgEIcHlwaS5vcmc[0-9A-Za-z_\-]{16,}`, "pypi-"),
	pat(`(?:sk|rk)_live_[0-9A-Za-z]{16,}`, "_live_"),
	pat(`AIza[0-9A-Za-z_\-]{35}`, "aiza"),
	pat(`hvs\.[0-9A-Za-z_\-]{24,}`, "hvs."),
	// JSON web tokens: a header and a payload that are base64 JSON.
	pat(`eyJ[0-9A-Za-z_\-]{8,}\.eyJ[0-9A-Za-z_\-]{8,}\.[0-9A-Za-z_\-]*`, "eyj"),
	// Private keys, whole blocks.
	pat(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`, "private key"),
}

// Secrets that keep their context. Each has three groups: what is kept
// before the secret, the secret, what is kept after it.
var contextPatterns = []secretPattern{
	// An AWS access key id: exactly 20 capitals and digits, which may
	// follow a lower-case letter but not another capital or digit.
	pat(`(^|[^0-9A-Z])((?:AKIA|ASIA)[0-9A-Z]{16})\b()`, "akia", "asia"),
	// The password of a URL, and a token as a URL's only user information.
	pat(`(://[^/\s:@]+:)([^/\s@]+)(@)`, "://"),
	pat(`(://)([0-9A-Za-z_.~\-]{20,})(@)`, "://"),
	// An Authorization header of any scheme.
	pat(`(?i)(authorization:[ \t]*(?:(?:basic|bearer|token|digest|negotiate|bot|apikey)[ \t]+)?)([^\s"']+)()`, "authorization"),
	// A registry's credentials in a Docker or similar JSON configuration.
	pat(`(?i)("(?:auth|password|identitytoken|registrytoken|token|secret)"[ \t]*:[ \t]*")([^"\s]+)(")`, "auth\"", "password\"", "token\"", "secret\""),
	// A URL's query parameter named like a secret.
	pat(`(?i)([?&][0-9A-Za-z_.\-]*(?:token|secret|passw|signature|sig|key|auth|credential)[0-9A-Za-z_.\-]*=)([^&\s"'#]+)()`, "token", "secret", "passw", "sig", "key", "auth", "credential"),
	// An assignment, option or header named like a secret
	// (AWS_SECRET_ACCESS_KEY=..., --password=..., PRIVATE-TOKEN: ...). A
	// value that is a variable ($TOKEN) is no secret.
	pat(`(?i)((?:^|[\s"'(,;&|:/])-{0,2}[0-9A-Za-z_.\-]*(?:password|passwd|secret|token|api[_\-]?key|access[_\-]?key|private[_\-]?key)[0-9A-Za-z_.\-]*[ \t]*[=:][ \t]*)([^\s"'$&;,)][^\s"'&;,)]*)()`, "passw", "secret", "token", "key"),
}

// secretPattern is a secret's format, with the texts (in lower case) of
// which a match holds at least one: a text that has none is not searched.
type secretPattern struct {
	re    *regexp.Regexp
	hints []string
}

func pat(expr string, hints ...string) secretPattern {
	return secretPattern{re: regexp.MustCompile(expr), hints: hints}
}

// in reports whether a text, in lower case, may hold a match.
func (p secretPattern) in(lower string) bool {
	for _, h := range p.hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

var secretArgName = regexp.MustCompile(`(?i)token|secret|passw|pwd|key|credential|auth|cookie|session`)

// SecretArg reports whether a build argument's name says it holds a secret.
// Its value is never shown, and it is masked wherever it appears.
func SecretArg(name string) bool {
	return secretArgName.MatchString(name)
}

// The halves of a private key block that a part of a longer text can hold
// once the whole blocks are masked: up to its last END, from its BEGIN on.
var (
	keyEnd   = regexp.MustCompile(`(?s)\A.*-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	keyBegin = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*\z`)
)

// MaskSecrets hides the known secret formats (tokens, keys, passwords in
// URLs, bearer tokens) in text that did not come from a build, such as a
// CI job's log. A job's log is read in parts, so a key may be cut in two
// by a part's edge: each half is masked as well.
func MaskSecrets(s string) string {
	s = mask(s, nil)
	s = keyEnd.ReplaceAllString(s, Masked)
	return keyBegin.ReplaceAllString(s, Masked)
}

// MaskDockerfile is MaskSecrets for a Dockerfile's text, which is shown
// by line number: a masked secret that spans lines (a private key) keeps
// them, empty, so the lines after it keep their numbers.
func MaskDockerfile(text string) string { return maskText(text, nil, true) }

// mask hides known secret formats and the values of secret build arguments.
// It is best effort: BuildKit does not know which output is secret.
func mask(s string, args map[string]string) string { return maskText(s, args, false) }

// maskText is mask; keepLines keeps the line breaks of what it masks.
func maskText(s string, args map[string]string, keepLines bool) string {
	return maskWith(s, secretValues(args), func(secret string) string {
		if keepLines {
			return Masked + strings.Repeat("\n", strings.Count(secret, "\n"))
		}
		return Masked
	})
}

// secretValues are the values of the secret build arguments, as masking
// looks for them: each also as it reads URL-encoded, base64-encoded or
// escaped in JSON, a value of several lines also line by line (a step's
// live tail sees one line at a time); longest first, so a value containing
// another is masked whole.
func secretValues(args map[string]string) []string {
	seen := map[string]bool{}
	var values []string
	add := func(v string) {
		if len(strings.TrimSpace(v)) >= 4 && !seen[v] {
			seen[v] = true
			values = append(values, v)
		}
	}
	for k, v := range args {
		if !SecretArg(k) || len(v) < 4 {
			continue
		}
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(base64.RawURLEncoding.EncodeToString([]byte(v)))
		if j, err := json.Marshal(v); err == nil {
			j := string(j[1 : len(j)-1])
			add(j)
			// Also as an encoder that keeps <, > and & writes it.
			add(strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(j))
		}
		if strings.Contains(v, "\n") {
			for _, l := range strings.Split(v, "\n") {
				add(strings.TrimRight(l, "\r"))
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

// maskWith replaces the given values and the known secret formats by
// what repl makes of each secret.
func maskWith(s string, values []string, repl func(secret string) string) string {
	for _, v := range values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, repl(v))
		}
	}
	// What repl puts in holds none of the hints.
	lower := strings.ToLower(s)
	for _, p := range secretPatterns {
		if p.in(lower) {
			s = p.re.ReplaceAllStringFunc(s, repl)
		}
	}
	for _, p := range contextPatterns {
		if !p.in(lower) {
			continue
		}
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			g := p.re.FindStringSubmatch(m)
			if len(g) != 4 || g[2] == Masked || strings.HasPrefix(g[2], keyedPrefix) {
				return m
			}
			return g[1] + repl(g[2]) + g[3]
		})
	}
	return s
}

// ansi matches a terminal's control sequences: CSI (colours, cursor
// movement), OSC (titles, links, ended by BEL or ST) and the other
// two-byte escapes. Tools that force colour print them into a build's
// log, where they would show as text.
var ansi = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// stripANSI removes a terminal's control sequences from a log's text.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansi.ReplaceAllString(s, "")
}

// clip keeps the head and the tail of an output longer than MaxStepOutput.
// Masking runs before clipping, so no secret is cut in half and left
// readable.
func clip(s string) string {
	if len(s) <= MaxStepOutput {
		return s
	}
	return clipped(s[:outputHead], int64(len(s)-MaxStepOutput), s[len(s)-(MaxStepOutput-outputHead):])
}

func clipped(head string, cut int64, tail string) string {
	return strings.ToValidUTF8(head, "") + fmt.Sprintf("\n[… %d bytes cut …]\n", cut) + strings.ToValidUTF8(tail, "")
}

// A log is read keeping its head and tail only, as much as clip shows and
// a margin on each side: masking then still sees whole a secret that
// straddles where clip cuts (the longest, a private key, is a few KiB).
const (
	logMargin = 16 << 10
	keptHead  = outputHead + logMargin
	keptTail  = MaxStepOutput - outputHead + logMargin
)

// logBuffer keeps the head and the tail of a log written to it, and counts
// the bytes between them that it dropped.
type logBuffer struct {
	head, tail []byte
	cut        int64
	// budget, when set, is what the logs of one blob may still keep; a log
	// whose tail it cannot hold keeps its head and only counts the rest.
	budget   *int64
	reserved bool // the tail's room is taken from the budget
	headOnly bool
}

func (l *logBuffer) Write(p []byte) {
	if n := min(len(p), keptHead-len(l.head)); n > 0 && !l.headOnly {
		if l.budget != nil {
			n = int(min(int64(n), max(*l.budget, 0)))
			*l.budget -= int64(n)
		}
		l.head = append(l.head, p[:n]...)
		p = p[n:]
		if len(p) > 0 && len(l.head) < keptHead {
			l.headOnly = true
		}
	}
	if len(p) == 0 {
		return
	}
	if l.budget != nil && !l.reserved && !l.headOnly {
		// The tail grows to twice its size before it is cut back.
		if *l.budget < 2*keptTail {
			l.headOnly = true
		} else {
			*l.budget -= 2 * keptTail
			l.reserved = true
		}
	}
	if l.headOnly {
		l.cut += int64(len(p))
		return
	}
	if len(p) > keptTail {
		l.cut += int64(len(l.tail) + len(p) - keptTail)
		l.tail, p = l.tail[:0], p[len(p)-keptTail:]
	}
	l.tail = append(l.tail, p...)
	// Dropped in batches, so that a log written in small pieces is not
	// copied for every one of them.
	if len(l.tail) > 2*keptTail {
		n := len(l.tail) - keptTail
		l.cut += int64(n)
		l.tail = append(l.tail[:0], l.tail[n:]...)
	}
}

// trim drops what the buffer keeps beyond its head and tail.
func (l *logBuffer) trim() {
	if n := len(l.tail) - keptTail; n > 0 {
		l.cut += int64(n)
		l.tail = append(l.tail[:0], l.tail[n:]...)
	}
}

// output is the log as a step's output: masked, then clipped. Terminal
// control sequences are removed first.
func (l *logBuffer) output(args map[string]string) string {
	if l.cut == 0 {
		return clip(mask(stripANSI(string(l.head)+string(l.tail)), args))
	}
	head, cut := string(l.head), l.cut
	if len(head) < keptHead {
		// The budget cut the head at any byte, with no margin after it:
		// its last line may hold the start of a secret that masking no
		// longer recognises, and a key it begins has no END in the head.
		n := strings.LastIndexByte(head, '\n') + 1
		head, cut = head[:n], cut+int64(len(head)-n)
	}
	head = keyBegin.ReplaceAllString(mask(stripANSI(head), args), Masked)
	tail := keyEnd.ReplaceAllString(mask(stripANSI(string(l.tail)), args), Masked)
	total := int64(len(head)+len(tail)) + cut
	// The margins keep the cut ends of the head and the tail out of what
	// is shown, unless masking shortened them by more than a margin (the
	// same token on many lines): the part of a secret that a cut leaves,
	// which masking no longer recognises, is then left out as well.
	end := min(len(head), outputHead, max(lastBreak(head), len(head)-logMargin))
	start := max(len(tail)-min(len(tail), MaxStepOutput-outputHead), min(firstBreak(tail), logMargin))
	head, tail = head[:end], tail[start:]
	return clipped(head, total-int64(len(head)+len(tail)), tail)
}

// lastBreak is where the last line of s begins (after its last line
// break, else after its last space): a secret cut at the end of s lies
// after it. 0 when s has neither.
func lastBreak(s string) int {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return i + 1
	}
	return strings.LastIndexAny(s, " \t\r") + 1
}

// firstBreak is where the first whole line of s begins (after its first
// line break, else after its first space): a secret cut at the start of s
// lies before it. len(s) when s has neither.
func firstBreak(s string) int {
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		i = strings.IndexAny(s, " \t\r")
	}
	if i < 0 {
		return len(s)
	}
	return i + 1
}
