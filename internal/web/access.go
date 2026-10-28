package web

import (
	"log/slog"
	"net/http"
	"time"
)

// slowPage is when a page is worth a line at the info level.
const slowPage = 2 * time.Second

// AccessLog logs every request at the debug level (method, path without
// its query, status, time, bytes), a page slower than slowPage at info
// and a server error at warn. The query is never logged: an OAuth
// callback carries its code and state there. Event streams are long by
// design and only logged when they end.
func AccessLog(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		dur := time.Since(start)
		args := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "dur_ms", dur.Milliseconds(), "bytes", rec.bytes}
		switch {
		// 503 is the web starting (waiting for its controller): expected.
		case rec.status >= 500 && rec.status != http.StatusServiceUnavailable:
			log.Warn("request failed", args...)
		case dur > slowPage && r.URL.Path != "/events":
			log.Info("slow request", args...)
		default:
			log.Debug("request", args...)
		}
	})
}

// recorder notes a response's status and size; it passes Flush on, which
// the event streams need.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection underneath.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
