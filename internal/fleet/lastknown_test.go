package fleet

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLastKnown(t *testing.T) {
	l := NewLastKnown(3, 1<<20)
	pages := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": hello\n\n")
		case "/missing":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(404)
			fmt.Fprint(w, "<p>no</p>")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<div class="page">`+r.URL.Path+`</div>`)
		}
	})
	get := func(key, path string) string {
		rec := httptest.NewRecorder()
		l.Record(key, pages).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}
	if b := get("eu\x00alice\x00/builds", "/builds"); !strings.Contains(b, "/builds") {
		t.Fatalf("the page itself: %q", b)
	}
	if b, ct, at, ok := l.Get("eu\x00alice\x00/builds"); !ok || string(b) != `<div class="page">/builds</div>` || !strings.HasPrefix(ct, "text/html") || at.IsZero() {
		t.Fatalf("kept %q %q %v %v", b, ct, at, ok)
	}
	if _, _, _, ok := l.Get("eu\x00bob\x00/builds"); ok {
		t.Fatal("another person's copy")
	}
	get("eu\x00alice\x00/events", "/events")
	get("eu\x00alice\x00/missing", "/missing")
	for _, k := range []string{"eu\x00alice\x00/events", "eu\x00alice\x00/missing"} {
		if _, _, _, ok := l.Get(k); ok {
			t.Errorf("kept %q", k)
		}
	}
	rec := httptest.NewRecorder()
	l.Record("eu\x00alice\x00/post", pages).ServeHTTP(rec, httptest.NewRequest("POST", "/post", nil))
	if _, _, _, ok := l.Get("eu\x00alice\x00/post"); ok {
		t.Error("kept a POST")
	}
	// The least recently used goes first; Forget drops a member's pages.
	get("eu\x00alice\x00/a", "/a")
	get("us\x00alice\x00/b", "/b")
	l.Get("eu\x00alice\x00/builds")
	get("eu\x00alice\x00/c", "/c")
	if _, _, _, ok := l.Get("eu\x00alice\x00/a"); ok {
		t.Error("the least recently used page was kept over the limit")
	}
	if _, _, _, ok := l.Get("eu\x00alice\x00/builds"); !ok {
		t.Error("a page used again was dropped")
	}
	l.Forget("eu\x00")
	if _, _, _, ok := l.Get("eu\x00alice\x00/builds"); ok {
		t.Error("Forget kept the member's pages")
	}
	if _, _, _, ok := l.Get("us\x00alice\x00/b"); !ok {
		t.Error("Forget dropped another member's page")
	}
	// A page larger than a quarter of the bound is not kept.
	big := NewLastKnown(10, 100)
	rec = httptest.NewRecorder()
	big.Record("k", pages).ServeHTTP(rec, httptest.NewRequest("GET", "/"+strings.Repeat("x", 60), nil))
	if _, _, _, ok := big.Get("k"); ok {
		t.Error("kept a page over the size bound")
	}
}

// Keys count against the bound like pages; a page with a very long address,
// or one that grows past the size bound while it is sent, is not kept.
func TestLastKnownCountsKeys(t *testing.T) {
	l := NewLastKnown(100, 4000)
	key := func(i int) string { return fmt.Sprintf("eu\x00alice\x00/%d?%s", i, strings.Repeat("q", 1000)) }
	for i := 0; i < 4; i++ {
		l.put(key(i), []byte("<p>page</p>"), "text/html")
	}
	if _, _, _, ok := l.Get(key(0)); ok {
		t.Errorf("4 keys of 1 KiB were kept under a 4000-byte bound (accounted %d)", l.bytes)
	}
	if _, _, _, ok := l.Get(key(3)); !ok {
		t.Error("the newest page was dropped")
	}
	long := "eu\x00alice\x00/x?" + strings.Repeat("q", maxKeyLen)
	roomy := NewLastKnown(100, 1<<20)
	roomy.put(long, []byte("<p>page</p>"), "text/html")
	if _, _, _, ok := roomy.Get(long); ok {
		t.Error("kept a page under an address longer than the key bound")
	}
	rec := &recorder{ResponseWriter: httptest.NewRecorder(), limit: 100}
	rec.Header().Set("Content-Type", "text/html")
	for i := 0; i < 10; i++ {
		rec.Write([]byte(strings.Repeat("x", 30)))
	}
	if rec.keep || rec.buf.Len() > 100 {
		t.Errorf("the recorder holds %d bytes over its limit of 100 (keep %v)", rec.buf.Len(), rec.keep)
	}
}
