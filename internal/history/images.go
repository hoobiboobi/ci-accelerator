package history

import "strings"

// Repository is an image name without its tag or digest:
// "registry.example.com/group/app:1.2@sha256:..." -> "registry.example.com/group/app".
// A colon before the last slash is a registry port, not a tag.
func Repository(name string) string {
	name, _, _ = strings.Cut(name, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// ImageNames are the image names of exporters' name attributes, one by
// one: an image exporter takes several names in one attribute, joined
// with commas ("reg/g/app:abc,reg/g/app:latest", as the CI component
// pushes a commit's tag and latest).
func ImageNames(attrs []string) []string {
	var out []string
	for _, a := range attrs {
		for n := range strings.SplitSeq(a, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// Repositories are the repositories of images (exporters' name
// attributes), once each, in their order.
func Repositories(images []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range ImageNames(images) {
		r := Repository(n)
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
