package gitlab

import "strings"

// CleanLog turns a whole job log as GitLab stores it into the text it
// shows: the reference the log parts are checked against.
func CleanLog(s string) string {
	s = logSection.ReplaceAllString(s, "")
	s = logANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l, cont := cleanLine(l)
		// No empty piece around a section marker, no run of empty lines.
		if l == "" && (cont || (len(out) > 0 && out[len(out)-1] == "")) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
