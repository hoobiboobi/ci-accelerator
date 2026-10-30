package history

import (
	"fmt"
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
var secretPatterns = []*regexp.Regexp{
	// GitLab tokens, by their documented prefixes.
	regexp.MustCompile(`(?:glpat|gloas|gldt|glrtr|glrt|glcbt|glptt|glft|glimt|glagent|glwt|glsoat|glffct)-[0-9A-Za-z_.\-]{16,}`),
	regexp.MustCompile(`_gitlab_session=[0-9A-Za-z]+`),
	// GitHub tokens.
	regexp.MustCompile(`gh[pousr]_[0-9A-Za-z]{36,}`),
	regexp.MustCompile(`github_pat_[0-9A-Za-z_]{22,}`),
	// Slack tokens.
	regexp.MustCompile(`xox[abposr]-[0-9A-Za-z\-]{10,}`),
	// Private keys, whole blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
}

// Secrets that keep their context: the password of a URL, a bearer token,
// an AWS access key id (exactly 20 capitals and digits, which may follow
// a lower-case letter but not another capital or digit).
var (
	awsKeyID    = regexp.MustCompile(`(^|[^0-9A-Z])(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	urlPassword = regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`)
	bearer      = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s"']+`)
)

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
	masked := func(secret string) string {
		if keepLines {
			return Masked + strings.Repeat("\n", strings.Count(secret, "\n"))
		}
		return Masked
	}
	var values []string
	for k, v := range args {
		if !SecretArg(k) || len(v) < 4 {
			continue
		}
		values = append(values, v)
		// A value of several lines is also masked line by line: a step's
		// live tail sees one line at a time.
		if strings.Contains(v, "\n") {
			for _, l := range strings.Split(v, "\n") {
				if l = strings.TrimRight(l, "\r"); len(strings.TrimSpace(l)) >= 4 {
					values = append(values, l)
				}
			}
		}
	}
	// Longest first, so a value containing another is masked whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		s = strings.ReplaceAll(s, v, masked(v))
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, masked)
	}
	s = awsKeyID.ReplaceAllString(s, "${1}"+Masked)
	s = urlPassword.ReplaceAllString(s, "${1}"+Masked+"@")
	return bearer.ReplaceAllString(s, "${1}"+Masked)
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

// output is the log as a step's output: masked, then clipped.
func (l *logBuffer) output(args map[string]string) string {
	if l.cut == 0 {
		return clip(mask(string(l.head)+string(l.tail), args))
	}
	head, cut := string(l.head), l.cut
	if len(head) < keptHead {
		// The budget cut the head at any byte, with no margin after it:
		// its last line may hold the start of a secret that masking no
		// longer recognises, and a key it begins has no END in the head.
		n := strings.LastIndexByte(head, '\n') + 1
		head, cut = head[:n], cut+int64(len(head)-n)
	}
	head = keyBegin.ReplaceAllString(mask(head, args), Masked)
	tail := mask(string(l.tail), args)
	total := int64(len(head)+len(tail)) + cut
	head = head[:min(len(head), outputHead)]
	tail = tail[len(tail)-min(len(tail), MaxStepOutput-outputHead):]
	return clipped(head, total-int64(len(head)+len(tail)), tail)
}
