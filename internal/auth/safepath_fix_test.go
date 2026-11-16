package auth

import "testing"

// A return address is sealed into the login cookie and sent back as the
// Location of the redirect after sign-in: no control character may pass,
// not even in the fragment, where url.Parse does not look.
func TestSafePathRefusesControlCharacters(t *testing.T) {
	for _, in := range []string{
		"/#\x00", "/builds#\x1b[31m", "/#\x7f", "/#\x0b", "/builds\x00", "/b?x=\x01",
		"/#\u0085", "/#\u009b31m", // C1: NEL, CSI
		"/builds#\u202etxt.exe", "/builds\u2066x\u2069", "/#\u202a", // bidi overrides and isolates
	} {
		if got := SafePath(in); got != "/" {
			t.Errorf("SafePath(%q) = %q, want /", in, got)
		}
	}
	for _, in := range []string{"/builds#top", "/m/eu/pipelines/abc/7?f=x", "/p/café", "/# "} {
		if got := SafePath(in); got != in {
			t.Errorf("SafePath(%q) = %q, want it unchanged", in, got)
		}
	}
}
