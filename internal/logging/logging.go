// Package logging is how every role of ci-accelerator writes its log: one
// line per event on stderr, JSON (or text for reading by eye), at a level
// set by a flag, with secrets kept out of it whatever a caller passes.
package logging

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Flags are the log's flags of every role.
type Flags struct {
	Level, Format string
}

// Register adds -log-level and -log-format to a role's flags.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.Level, "log-level", "info", "the least important events logged: debug, info, warn or error")
	fs.StringVar(&f.Format, "log-format", "json", "json (one object per line) or text (key=value, for reading by eye)")
}

// New makes a role's logger writing to w; every line names the role.
func New(w io.Writer, f Flags, role string) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(f.Level) {
	case "debug":
		level = slog.LevelDebug
	case "", "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("-log-level %q: debug, info, warn or error", f.Level)
	}
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: Redact}
	var h slog.Handler
	switch strings.ToLower(f.Format) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("-log-format %q: json or text", f.Format)
	}
	l := slog.New(h)
	if role != "" {
		l = l.With("role", role)
	}
	return l, nil
}

// ErrorLog is a standard logger for an http.Server's own errors (TLS
// handshakes, panics in a handler), as warnings of l.
func ErrorLog(l *slog.Logger) *log.Logger { return slog.NewLogLogger(l.Handler(), slog.LevelWarn) }

// secretKey is an attribute whose value is a credential by its name.
var secretKey = regexp.MustCompile(`(?i)token|secret|cookie|authorization|password|passwd|credential|access_key|private_key|session`)

// urlQuery is the query of an address in a text: it may carry a token
// (an OAuth callback's code and state, a token parameter).
var urlQuery = regexp.MustCompile(`(https?://[^\s"'?#]+)\?[^\s"'#]*`)

// Redact is the handlers' ReplaceAttr: a value whose key names a
// credential is never written, and every string has known token formats
// masked and the queries of the addresses in it cut.
func Redact(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
		return a
	}
	if a.Key != slog.MessageKey && secretKey.MatchString(a.Key) {
		return slog.String(a.Key, "[redacted]")
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, clean(v.String()))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, clean(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, clean(x.String()))
		case []string:
			out := make([]string, len(x))
			for i, s := range x {
				out[i] = clean(s)
			}
			return slog.Any(a.Key, out)
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

func clean(s string) string {
	return urlQuery.ReplaceAllString(history.MaskSecrets(s), "$1?[redacted]")
}

// Every lets a repeating event through once per period per key, and says
// how many it held back since: a daemon or GitLab that is down for an hour
// writes sixty lines, not thousands.
type Every struct {
	Period time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
	held   map[string]int
	now    func() time.Time
}

// Allow says whether to log key now, and how many were held back before.
func (e *Every) Allow(key string) (bool, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		e.last, e.held = map[string]time.Time{}, map[string]int{}
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	if t, ok := e.last[key]; ok && now.Sub(t) < e.Period {
		e.held[key]++
		return false, 0
	}
	held := e.held[key]
	e.last[key], e.held[key] = now, 0
	return true, held
}

// Warn logs a warning through e: at most once a period per msg and key,
// with "suppressed" saying how many were held back.
func (e *Every) Warn(l *slog.Logger, key, msg string, args ...any) {
	ok, held := e.Allow(msg + "\x00" + key)
	if !ok {
		return
	}
	if held > 0 {
		args = append(args, "suppressed", held)
	}
	l.Warn(msg, args...)
}

// Discard is a logger that writes nothing, for code given none.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Canceled says whether err is only the end of the process (a context
// canceled at shutdown): not worth a warning.
func Canceled(err error) bool {
	return errors.Is(err, context.Canceled)
}
