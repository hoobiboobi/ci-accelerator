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
var secretPatterns = []*regexp.Regexp{
	// GitLab tokens, by their documented prefixes.
	regexp.MustCompile(`\b(?:glpat|gloas|gldt|glrtr|glrt|glcbt|glptt|glft|glimt|glagent|glwt|glsoat|glffct)-[0-9A-Za-z_.\-]{16,}`),
	regexp.MustCompile(`_gitlab_session=[0-9A-Za-z]+`),
	// GitHub tokens.
	regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{36,}`),
	regexp.MustCompile(`\bgithub_pat_[0-9A-Za-z_]{22,}`),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[abposr]-[0-9A-Za-z\-]{10,}`),
	// Private keys, whole blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

// Secrets that keep their context: the password of a URL, a bearer token.
var (
	urlPassword = regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`)
	bearer      = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s"']+`)
)

var secretArgName = regexp.MustCompile(`(?i)token|secret|passw|pwd|key|credential|auth|cookie|session`)

// SecretArg reports whether a build argument's name says it holds a secret.
// Its value is never shown, and it is masked wherever it appears.
func SecretArg(name string) bool {
	return secretArgName.MatchString(name)
}

// MaskSecrets hides the known secret formats (tokens, keys, passwords in
// URLs, bearer tokens) in text that did not come from a build, such as a
// CI job's log.
func MaskSecrets(s string) string { return mask(s, nil) }

// mask hides known secret formats and the values of secret build arguments.
// It is best effort: BuildKit does not know which output is secret.
func mask(s string, args map[string]string) string {
	var values []string
	for k, v := range args {
		if SecretArg(k) && len(v) >= 4 {
			values = append(values, v)
		}
	}
	// Longest first, so a value containing another is masked whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		s = strings.ReplaceAll(s, v, Masked)
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Masked)
	}
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
	cut := len(s) - MaxStepOutput
	head, tail := s[:outputHead], s[len(s)-(MaxStepOutput-outputHead):]
	return strings.ToValidUTF8(head, "") + fmt.Sprintf("\n[… %d bytes cut …]\n", cut) + strings.ToValidUTF8(tail, "")
}
