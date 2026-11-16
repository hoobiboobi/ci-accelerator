package fleet

import (
	"bytes"
	"container/list"
	"net/http"
	"sync"
	"time"
)

// LastKnown keeps the last copy of the pages people opened, per member and
// per person, so that a member that is away still shows what was last seen
// of it, dated. Bounded: the least recently used pages go first, and one
// person's pages of one member (keys member\x00person\x00...) take at most
// a tenth of MaxPages (at least 10), so that nobody evicts everyone else's.
type LastKnown struct {
	// MaxPages and MaxBytes bound what is kept.
	MaxPages int
	MaxBytes int

	mu    sync.Mutex
	order *list.List // of *kept, the most recent in front
	byKey map[string]*list.Element
	bytes int
	per   map[string]int // pages kept per owner (see owner)
}

type kept struct {
	key   string
	owner string
	at    time.Time
	body  []byte
	ct    string
}

// maxKeyLen bounds the address of a kept page: a key is counted in
// MaxBytes like the page itself, and a longer one is not kept at all.
const maxKeyLen = 4 << 10

// NewLastKnown keeps at most pages pages and size bytes.
func NewLastKnown(pages, size int) *LastKnown {
	return &LastKnown{MaxPages: pages, MaxBytes: size, order: list.New(), byKey: map[string]*list.Element{}, per: map[string]int{}}
}

// owner is the part of key up to its second NUL (member\x00person\x00);
// "" when it has none, and such a key has no bound of its own.
func owner(key string) string {
	n := 0
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			if n++; n == 2 {
				return key[:i+1]
			}
		}
	}
	return ""
}

// Record serves h and keeps a successful HTML page it answers under key.
func (l *LastKnown) Record(key string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			h.ServeHTTP(w, r)
			return
		}
		rec := &recorder{ResponseWriter: w, limit: l.MaxBytes / 4}
		h.ServeHTTP(rec, r)
		if rec.code == http.StatusOK && rec.keep && rec.buf.Len() > 0 {
			l.put(key, rec.buf.Bytes(), rec.Header().Get("Content-Type"))
		}
	})
}

// Get is the page kept under key and when it was seen.
func (l *LastKnown) Get(key string) (body []byte, contentType string, at time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok {
		return nil, "", time.Time{}, false
	}
	l.order.MoveToFront(e)
	k := e.Value.(*kept)
	return k.body, k.ct, k.at, true
}

// Forget drops every page kept for keys starting with prefix (a member
// removed).
func (l *LastKnown) Forget(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, e := range l.byKey {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			l.drop(e)
		}
	}
}

func (l *LastKnown) put(key string, body []byte, ct string) {
	if len(body) > l.MaxBytes/4 || len(key) > maxKeyLen {
		return
	}
	b := append([]byte(nil), body...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byKey[key]; ok {
		l.drop(e)
	}
	o := owner(key)
	l.byKey[key] = l.order.PushFront(&kept{key: key, owner: o, at: time.Now(), body: b, ct: ct})
	l.bytes += len(key) + len(b)
	if o != "" {
		l.per[o]++
		if l.per[o] > max(10, l.MaxPages/10) {
			for e := l.order.Back(); e != nil; e = e.Prev() {
				if e.Value.(*kept).owner == o {
					l.drop(e)
					break
				}
			}
		}
	}
	for l.order.Len() > l.MaxPages || l.bytes > l.MaxBytes {
		l.drop(l.order.Back())
	}
}

func (l *LastKnown) drop(e *list.Element) {
	k := e.Value.(*kept)
	l.order.Remove(e)
	delete(l.byKey, k.key)
	l.bytes -= len(k.key) + len(k.body)
	if k.owner != "" {
		if l.per[k.owner]--; l.per[k.owner] <= 0 {
			delete(l.per, k.owner)
		}
	}
}

// recorder copies an HTML answer while it is sent, up to limit bytes (a
// longer one is not kept); streams (events) and other types pass through
// untouched.
type recorder struct {
	http.ResponseWriter
	limit  int
	code   int
	keep   bool
	headed bool
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if !r.headed {
		r.headed, r.code = true, code
		ct := r.Header().Get("Content-Type")
		r.keep = len(ct) >= 9 && ct[:9] == "text/html" && r.Header().Get("Content-Encoding") == ""
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.headed {
		r.WriteHeader(http.StatusOK)
	}
	if r.keep {
		if r.buf.Len()+len(b) > r.limit {
			r.keep = false
			r.buf = bytes.Buffer{}
		} else {
			r.buf.Write(b)
		}
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
