package fleet

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FuzzLastKnown runs a byte-driven sequence of Record, Get and Forget
// operations against one LastKnown, with keys built as the hub builds them
// (member \x00 person \x00 request URI) from member names that overlap by
// prefix ("eu", "eu-west", "e") or are very long, several people, and
// pages of fuzzed sizes, types and status codes.
//
// Invariants, checked after every operation:
//   - the accounted bytes equal the sum of the kept keys and bodies and
//     never exceed MaxBytes; the page count never exceeds MaxPages; the
//     index, the list and the per-owner counts agree, and no owner with no
//     page is left in the per-owner map (it would grow without bound);
//   - one person's pages of one member never exceed max(10, MaxPages/10);
//   - Get(key) returns only the body last recorded under exactly that key
//     (never another person's, another member's or another address's page)
//     and nothing recorded before that key's member was forgotten;
//   - Forget(name+"\x00") drops every page of that member and keeps every
//     page of every other member, including one whose name starts with it;
//   - a page that fits (a successful HTML GET, at most a quarter of
//     MaxBytes, its key within the key bound, key and body within MaxBytes)
//     is kept right after it is recorded;
//   - Record passes the answer through to the browser unchanged.
func FuzzLastKnown(f *testing.F) {
	// maxPages, maxBytes, then ops (see the decoder below; lkRec, lkGet
	// and lkForget write them).
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	// Record, read it back, read another person's copy of it.
	f.Add(uint8(3), uint16(4000), cat(lkRec(0, 0, 1, 0, 10), lkGet(0, 0, 1), lkGet(0, 1, 1)))
	// Prefix-overlapping members: eu-west and eu, forget eu (and e).
	f.Add(uint8(20), uint16(60000), cat(lkRec(1, 1, 1, 0, 20), lkRec(0, 1, 1, 0, 20), lkRec(4, 1, 1, 0, 20),
		lkForget(0), lkGet(1, 1, 1), lkGet(0, 1, 1), lkForget(2), lkGet(4, 1, 1)))
	// Two people of one member, one address; an admin's copy and a viewer's.
	f.Add(uint8(20), uint16(60000), cat(lkRec(0, 1, 4, 0, 30), lkRec(0, 2, 4, 0, 31), lkRec(0, 3, 4, 0, 32),
		lkGet(0, 1, 4), lkGet(0, 2, 4), lkGet(0, 3, 4), lkGet(0, 4, 4)))
	// Keys near and past the key bound (audit: keys were not counted).
	f.Add(uint8(100), uint16(30000), cat(lkRec(6, 0, 1, 0, 200), lkRec(7, 0, 1, 0, 100), lkRec(5, 0, 0x11, 0x20, 100),
		lkGet(6, 0, 1), lkGet(7, 0, 1), lkForget(6)))
	// Many pages of one person: the per-owner bound.
	var many [][]byte
	for i := 0; i < 30; i++ {
		// eu, person 1, address i%7 with a query of i q: 30 addresses.
		many = append(many, []byte{0, 0, 1, 0x10 | byte(i%7), byte(i), 0, 8, byte(i)})
	}
	many = append(many, lkRec(1, 1, 1, 0, 8), lkGet(1, 1, 1))
	f.Add(uint8(200), uint16(60000), cat(many...))
	// A non-HTML answer, a 404, a POST, a gzip one, a 204 over a kept page.
	f.Add(uint8(10), uint16(60000), cat(lkRec(0, 0, 1, 0x40, 9), lkRec(0, 0, 1, 0x80, 9), lkRec(0, 0, 1, 0xc0, 9),
		lkRec(0, 0, 1, 0xc1, 9), lkRec(0, 0, 2, 0, 9), lkRec(0, 0, 2, 0xc3, 9), lkGet(0, 0, 2)))
	// Pages grown in chunks past a quarter of the bound.
	f.Add(uint8(10), uint16(1000), cat(lkRec(0, 0, 1, 0x0c, 240), lkRec(0, 0, 1, 0x2c, 3), lkGet(0, 0, 1)))
	// Zero bounds.
	f.Add(uint8(0), uint16(0), cat(lkRec(0, 0, 0, 0, 5), lkGet(0, 0, 0)))
	f.Fuzz(func(t *testing.T, maxPages uint8, maxBytes uint16, ops []byte) {
		l := NewLastKnown(int(maxPages), int(maxBytes))
		r := &lkReader{b: ops}
		want := map[string][]byte{} // what Get may return per key
		for step := 0; step < 200 && !r.done(); step++ {
			switch r.byte() % 4 {
			case 0, 3: // Record
				key, name := lkKey(r)
				spec := r.byte()
				n := int(r.byte())
				if spec&0x20 != 0 {
					n = n*64 + int(r.byte())
				}
				body := lkBody(r, n)
				ct, status, method, enc := "text/html; charset=utf-8", http.StatusOK, http.MethodGet, ""
				switch spec >> 6 {
				case 1:
					ct = "text/event-stream"
				case 2:
					status = http.StatusNotFound
				case 3:
					switch spec & 3 {
					case 0:
						method = http.MethodPost
					case 1:
						enc = "gzip"
					case 2:
						ct = "application/json"
					default:
						status = http.StatusNoContent
					}
				}
				chunks := 1 + int(spec>>2&3)
				h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", ct)
					if enc != "" {
						w.Header().Set("Content-Encoding", enc)
					}
					if status != http.StatusOK || spec&0x10 != 0 {
						w.WriteHeader(status)
					}
					if status == http.StatusNoContent {
						return
					}
					for i := 0; i < chunks; i++ {
						lo, hi := i*len(body)/chunks, (i+1)*len(body)/chunks
						w.Write(body[lo:hi])
					}
				})
				rec := httptest.NewRecorder()
				l.Record(key, h).ServeHTTP(rec, httptest.NewRequest(method, "/x", nil))
				if status != http.StatusNoContent && !bytes.Equal(rec.Body.Bytes(), body) {
					t.Fatalf("Record changed the answer: sent %d bytes, the browser got %d", len(body), rec.Body.Len())
				}
				keep := method == http.MethodGet && status == http.StatusOK && enc == "" &&
					strings.HasPrefix(ct, "text/html") && len(body) > 0 && len(body) <= l.MaxBytes/4 && len(key) <= maxKeyLen
				if keep {
					want[key] = append([]byte(nil), body...)
					if len(key)+len(body) <= l.MaxBytes && l.MaxPages >= 1 {
						if _, ok := l.byKey[key]; !ok {
							t.Fatalf("a page that fits was not kept: key %d bytes, body %d bytes, bounds %d pages %d bytes", len(key), len(body), l.MaxPages, l.MaxBytes)
						}
					}
				} else if method == http.MethodGet && status == http.StatusOK && enc == "" && strings.HasPrefix(ct, "text/html") && len(body) > 0 {
					// Not kept for its size: an earlier copy under the key may stay.
				} else if _, ok := l.byKey[key]; ok && !bytes.Equal(l.byKey[key].Value.(*kept).body, want[key]) {
					t.Fatalf("an answer that is not kept replaced the page under %q", lkShow(key))
				}
				_ = name
			case 1: // Get
				key, _ := lkKey(r)
				body, _, _, ok := l.Get(key)
				if ok {
					w, known := want[key]
					if !known {
						t.Fatalf("Get(%q) returned a page never recorded under that key (or its member was forgotten)", lkShow(key))
					}
					if !bytes.Equal(body, w) {
						t.Fatalf("Get(%q) returned a page other than the one last recorded under it", lkShow(key))
					}
				}
			case 2: // Forget
				name := lkName(r)
				before := map[string]bool{}
				for k := range l.byKey {
					before[k] = true
				}
				l.Forget(name + "\x00")
				for k := range before {
					_, still := l.byKey[k]
					mine := strings.HasPrefix(k, name+"\x00")
					if mine && still {
						t.Fatalf("Forget(%q) kept %q", lkShow(name), lkShow(k))
					}
					if !mine && !still {
						t.Fatalf("Forget(%q) dropped another member's page %q", lkShow(name), lkShow(k))
					}
				}
				for k := range want {
					if strings.HasPrefix(k, name+"\x00") {
						delete(want, k)
					}
				}
			}
			lkCheck(t, l)
		}
	})
}

// lkCheck verifies the bounds and the bookkeeping of l.
func lkCheck(t *testing.T, l *LastKnown) {
	t.Helper()
	if l.order.Len() != len(l.byKey) {
		t.Fatalf("the list holds %d pages, the index %d", l.order.Len(), len(l.byKey))
	}
	if l.order.Len() > l.MaxPages {
		t.Fatalf("%d pages kept over the bound of %d", l.order.Len(), l.MaxPages)
	}
	sum := 0
	per := map[string]int{}
	for e := l.order.Front(); e != nil; e = e.Next() {
		k := e.Value.(*kept)
		if l.byKey[k.key] != e {
			t.Fatalf("the index does not lead to the page %q", lkShow(k.key))
		}
		if k.owner != owner(k.key) {
			t.Fatalf("page %q counted for owner %q", lkShow(k.key), lkShow(k.owner))
		}
		sum += len(k.key) + len(k.body)
		if k.owner != "" {
			per[k.owner]++
		}
	}
	if sum != l.bytes {
		t.Fatalf("accounted %d bytes, the pages and keys hold %d", l.bytes, sum)
	}
	if l.bytes > l.MaxBytes {
		t.Fatalf("%d bytes kept over the bound of %d", l.bytes, l.MaxBytes)
	}
	if len(per) != len(l.per) {
		t.Fatalf("the per-owner counts hold %d owners, the pages %d", len(l.per), len(per))
	}
	for o, n := range per {
		if l.per[o] != n {
			t.Fatalf("owner %q counted %d pages, holds %d", lkShow(o), l.per[o], n)
		}
		if n > max(10, l.MaxPages/10) {
			t.Fatalf("owner %q holds %d pages over the per-owner bound %d", lkShow(o), n, max(10, l.MaxPages/10))
		}
	}
}

// lkReader turns the fuzzer's bytes into choices; past the end every
// choice is 0.
type lkReader struct {
	b []byte
	i int
}

func (r *lkReader) done() bool { return r.i >= len(r.b) }

func (r *lkReader) byte() byte {
	if r.i >= len(r.b) {
		return 0
	}
	r.i++
	return r.b[r.i-1]
}

// Member names as the registry allows them: overlapping prefixes, and long
// ones (a key near and past the key bound).
var lkNames = []string{"eu", "eu-west", "e", "us", "eu-west-1", "a" + strings.Repeat("b", 1000), "a" + strings.Repeat("b", 4090), "a" + strings.Repeat("b", 4100)}

// People as the hub writes them (host/user/admin), "-" without sign-in.
var lkPeople = []string{"-", "gitlab.com/1/false", "gitlab.com/1/true", "gitlab.com/12/false", "gitlab.com/2/false", "gitlab.co/1/false"}

var lkURIs = []string{"/", "/builds", "/builds?project=1", "/builds?project=12", "/pipelines/0a1b2c/7", "/jobs/0a1b2c/9", "/x"}

func lkName(r *lkReader) string { return lkNames[int(r.byte())%len(lkNames)] }

// lkKey is a key the hub builds: name, person and address; the address
// sometimes grows by fuzzed bytes (a long query).
func lkKey(r *lkReader) (key, name string) {
	name = lkName(r)
	who := lkPeople[int(r.byte())%len(lkPeople)]
	b := r.byte()
	uri := lkURIs[int(b&0x0f)%len(lkURIs)]
	if b&0x10 != 0 {
		uri += "&q=" + strings.Repeat("q", int(r.byte())*int(b>>5+1))
	}
	return name + "\x00" + who + "\x00" + uri, name
}

func lkBody(r *lkReader, n int) []byte {
	b := make([]byte, n)
	seed := r.byte()
	for i := range b {
		b[i] = 'a' + (seed+byte(i))%26
	}
	return b
}

// lkRec, lkGet and lkForget write the ops the decoder reads: a record
// under names[name], people[who], the address chosen by uri (its bit 0x10
// adds a query of 16 q), with the answer's spec byte and n body bytes (or
// n*64+1 with spec bit 0x20).
func lkRec(name, who, uri, spec, n byte) []byte {
	b := []byte{0, name, who, uri}
	if uri&0x10 != 0 {
		b = append(b, 16/(uri>>5+1))
	}
	b = append(b, spec, n)
	if spec&0x20 != 0 {
		b = append(b, 1)
	}
	return append(b, name+who+uri)
}

func lkGet(name, who, uri byte) []byte { return []byte{1, name, who, uri &^ 0x10} }

func lkForget(name byte) []byte { return []byte{2, name} }

// lkShow shortens a key for a message.
func lkShow(s string) string {
	s = strings.ReplaceAll(s, "\x00", `\0`)
	if len(s) > 80 {
		return s[:40] + "..." + s[len(s)-30:]
	}
	return s
}
