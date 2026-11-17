package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A redirect goes only to a path of the site: "//host" and "/\host" are
// another host's address to a browser, with or without a base path.
func TestRedirectStaysOnTheSite(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/builds/1": true, "/images/x?y=1": true,
		"": false, "//evil.example": false, `/\evil.example`: false, "https://evil.example/": false, "evil": false,
	} {
		if got := sitePath(p); got != want {
			t.Errorf("sitePath(%q) = %v, want %v", p, got, want)
		}
	}
	for _, base := range []string{"", "/m/eu"} {
		s := &Server{opt: Options{Base: base}}
		for _, to := range []string{"//evil.example/x", `/\evil.example/x`, "https://evil.example/"} {
			w := httptest.NewRecorder()
			s.redirect(w, httptest.NewRequest(http.MethodGet, "/", nil), to, http.StatusFound)
			if loc := w.Header().Get("Location"); loc != base+"/" {
				t.Errorf("base %q: a redirect to %q went to %q, want %q", base, to, loc, base+"/")
			}
		}
	}
	if got := safeBack(`/\evil.example`); got != "/" {
		t.Errorf("safeBack(/\\evil.example) = %q, want /", got)
	}
}
