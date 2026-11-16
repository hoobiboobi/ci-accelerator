package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// fuzzSessions is a store with two live sessions (alice, bob) whose
// cookies are fixed, so that seeds can name them.
type fuzzSessions struct {
	a          *Auth
	st         *store.Store
	cookies    [2][]byte
	users      [2]uint64
	ids        [2][]byte
	rows       [2]store.Session
	canonicals [2]string
}

func newFuzzSessions(f *testing.F) *fuzzSessions {
	f.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, f.TempDir(), store.Options{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { st.Close() })
	const gl = "https://gitlab.example.test"
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	a, err := New(Config{PublicURL: "https://console.example.test/", Key: bytes.Repeat([]byte{7}, 32),
		Now:       func() time.Time { return now },
		Instances: []Instance{{URL: gl, ClientID: "cid", Secret: "sec", Admins: []string{"id:1"}}}},
		st, gitlab.New([]string{gl}), nil)
	if err != nil {
		f.Fatal(err)
	}
	inst := history.InstancePrefix(gl)
	s := &fuzzSessions{a: a, st: st, users: [2]uint64{7, 1}}
	for i := range s.cookies {
		c := make([]byte, 32)
		for j := range c {
			c[j] = byte(i*32 + j)
		}
		s.cookies[i] = c
		s.ids[i] = hash(c)
		s.canonicals[i] = base64.RawURLEncoding.EncodeToString(c)
		sealed, err := a.sealTokens(s.ids[i], inst, tokens{Access: "at", Refresh: "rt", Expiry: now.Add(time.Hour)})
		if err != nil {
			f.Fatal(err)
		}
		if err := st.SaveSession(ctx, store.Session{ID: s.ids[i], Instance: inst, UserID: s.users[i],
			Username: []string{"alice", "root"}[i], Token: sealed, Created: now, Seen: now}); err != nil {
			f.Fatal(err)
		}
		if s.rows[i], err = st.Session(ctx, s.ids[i]); err != nil {
			f.Fatal(err)
		}
	}
	return s
}

// owner is the index of the session raw is the cookie of, by the bytes it
// decodes to (the secret), or -1.
func (s *fuzzSessions) owner(raw string) int {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return -1
	}
	for i, c := range s.cookies {
		if bytes.Equal(b, c) {
			return i
		}
	}
	return -1
}

// unchanged fails when a session row differs from how it was stored.
func (s *fuzzSessions) unchanged(t *testing.T, raw string) {
	t.Helper()
	for i, id := range s.ids {
		got, err := s.st.Session(context.Background(), id)
		if err != nil {
			t.Fatalf("cookie %q: session %d is gone or unreadable: %v", raw, i, err)
		}
		if !reflect.DeepEqual(got, s.rows[i]) {
			t.Fatalf("cookie %q: session %d was written:\n got %+v\nwant %+v", raw, i, got, s.rows[i])
		}
	}
}

// FuzzLoadCookie: sessions loaded from arbitrary cookie values.
//
// Invariants, for every value:
//   - LoadCookie never fails (the store is healthy) and never panics;
//   - it returns a viewer exactly when the value decodes to the 32 secret
//     bytes of a live session, and then that session's person, never the
//     other one;
//   - nothing is written: both session rows stay as stored (the clock
//     stands still, so even the right cookie touches nothing), and Load
//     sets no cookie;
//   - Load with the value as the whole Cookie header, and as the session
//     cookie's value, agrees with LoadCookie on what the browser sent.
func FuzzLoadCookie(f *testing.F) {
	s := newFuzzSessions(f)
	a, b := s.canonicals[0], s.canonicals[1]
	for _, seed := range []string{
		a, b, "", "=", a + "=", a + "A", a[:42], a[1:], strings.ToUpper(a), strings.ToLower(a),
		strings.ReplaceAll(strings.ReplaceAll(a, "-", "+"), "_", "/"),
		base64.StdEncoding.EncodeToString(s.cookies[0]),
		a[:42] + string(a[42]^1), a[:20] + "\n" + a[20:], a + "\r\n", " " + a, a + ";", a + "; " + SessionCookie + "=" + b,
		a + b, strings.Repeat("A", 43), strings.Repeat("A", 4096),
		"__Host-ci-session=" + a, SessionCookie + "=" + b + "; " + SessionCookie + "=" + a,
		"\x00" + a, a[:21] + "%" + a[22:], `"` + a + `"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ctx := context.Background()
		want := s.owner(raw)
		v, touched, err := s.a.LoadCookie(ctx, raw)
		if err != nil {
			t.Fatalf("LoadCookie(%q): %v", raw, err)
		}
		if touched {
			t.Fatalf("LoadCookie(%q) touched a session with the clock standing still", raw)
		}
		switch {
		case want < 0 && v != nil:
			t.Fatalf("LoadCookie(%q) = %s/%d, but the value is no live session's cookie", raw, v.Instance, v.UserID)
		case want >= 0 && v == nil:
			t.Fatalf("LoadCookie(%q): the cookie of session %d loaded nobody", raw, want)
		case want >= 0 && (v.UserID != s.users[want] || !bytes.Equal(v.id, s.ids[want])):
			t.Fatalf("LoadCookie(%q) = user %d, want session %d's user %d", raw, v.UserID, want, s.users[want])
		case v != nil && v.Admin != (v.UserID == 1):
			t.Fatalf("LoadCookie(%q): user %d admin=%v", raw, v.UserID, v.Admin)
		}
		s.unchanged(t, raw)

		// The same value through a request, as the whole Cookie header
		// and as the session cookie's value.
		for _, header := range []string{raw, SessionCookie + "=" + raw} {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Cookie", header)
			sent := -1
			if c, err := r.Cookie(SessionCookie); err == nil {
				sent = s.owner(c.Value)
			}
			w := httptest.NewRecorder()
			v, err := s.a.Load(w, r)
			if err != nil {
				t.Fatalf("Load(Cookie: %q): %v", header, err)
			}
			if (v != nil) != (sent >= 0) || (v != nil && v.UserID != s.users[sent]) {
				t.Fatalf("Load(Cookie: %q) = %+v, the browser sent session %d", header, v, sent)
			}
			if sc := w.Header().Values("Set-Cookie"); len(sc) != 0 {
				t.Fatalf("Load(Cookie: %q) set a cookie: %q", header, sc)
			}
			s.unchanged(t, header)
		}
	})
}

// FuzzSafePath: return addresses after a sign-in.
//
// Invariants, for every string:
//   - SafePath returns either "/" or its input unchanged, and is
//     idempotent;
//   - the result starts with exactly one "/", never "//" or "/\", has no
//     scheme, host or user info, and no control character (CR, LF, tab,
//     NUL, DEL, ...);
//   - it never starts with "/auth/" as written;
//   - sent as http.Redirect sends it, the Location a browser reads (tabs
//     and newlines removed, backslashes taken as slashes, as the WHATWG
//     URL parser does for http(s)) resolves against the console's address
//     to the console's own origin.
func FuzzSafePath(f *testing.F) {
	for _, seed := range []string{
		"/", "/builds", "/builds?project=42&ref=main#top", "/m/eu/pipelines/abc123/77", "/%2F%2Fevil.example",
		"", "builds", "//evil.example", "///evil.example", "/\\evil.example", "\\\\evil.example", "/\t/evil.example",
		"/\n/evil.example", "/\r\nSet-Cookie: x=y", "https://evil.example/", "javascript:alert(1)", "/javascript:alert(1)",
		"/auth/sign-in", "/auth/callback/abc123?code=x", "/./auth/sign-in", "/%61uth/sign-in", "/a/..//evil.example",
		"/..//evil.example", "/%2e%2e//evil.example", "/\x00", "/\x7f", "/\x0b/evil.example", "/ /evil.example",
		"/　/evil.example", "/／／evil.example", "/@evil.example", "/x@evil.example", "/:443", "/?//evil",
		"/#//evil", "/%", "/%zz", "/\xff\xfe",
		// Control characters after '#': url.Parse checks them only before
		// the fragment, so SafePath must refuse them itself.
		"/#\x00", "/builds#\x1b[31m", "/#\x7f", "/#\x0b",
	} {
		f.Add(seed)
	}
	base, err := url.Parse("https://console.example.test/m/eu/auth/callback/abc123")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, to string) {
		got := SafePath(to)
		if got != "/" && got != to {
			t.Fatalf("SafePath(%q) = %q: neither / nor the input", to, got)
		}
		if again := SafePath(got); again != got {
			t.Fatalf("SafePath(%q) = %q, but SafePath(%q) = %q", to, got, got, again)
		}
		if !strings.HasPrefix(got, "/") || strings.HasPrefix(got, "//") || strings.HasPrefix(got, "/\\") {
			t.Fatalf("SafePath(%q) = %q: not one leading slash", to, got)
		}
		if strings.HasPrefix(got, "/auth/") {
			t.Fatalf("SafePath(%q) = %q: a sign-in address", to, got)
		}
		for _, c := range []byte(got) {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("SafePath(%q) = %q: control character %#x", to, got, c)
			}
		}
		u, err := url.Parse(got)
		if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
			t.Fatalf("SafePath(%q) = %q: parses as scheme %q host %q (err %v)", to, got, u.Scheme, u.Host, err)
		}

		// What the browser does with the redirect.
		w := httptest.NewRecorder()
		http.Redirect(w, httptest.NewRequest("GET", "/auth/callback/abc123", nil), got, http.StatusFound)
		loc := w.Header().Get("Location")
		for _, c := range []byte(loc) {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("SafePath(%q) = %q: Location %q has control character %#x", to, got, loc, c)
			}
		}
		read := strings.NewReplacer("\t", "", "\n", "", "\r", "", "\\", "/").Replace(strings.Trim(loc, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "))
		if !strings.HasPrefix(read, "/") || strings.HasPrefix(read, "//") {
			t.Fatalf("SafePath(%q) = %q: the browser reads Location %q as %q, another origin", to, got, loc, read)
		}
		ref, err := url.Parse(read)
		if err != nil {
			return // the browser's parser would percent-encode; Go refuses: no origin to check
		}
		if r := base.ResolveReference(ref); r.Scheme != base.Scheme || r.Host != base.Host || r.User != nil {
			t.Fatalf("SafePath(%q) = %q: Location %q resolves to %s", to, got, loc, r)
		}
	})
}
