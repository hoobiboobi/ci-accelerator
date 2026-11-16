// Package api serves the console's read-only JSON API under /api/v1 and
// its health check. It has no authentication of its own: with sign-in the
// console lets only admins reach it; without, it is as open as the pages.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Store is what the API reads.
type Store interface {
	Recent(ctx context.Context, limit int) ([]*store.Stored, error)
	BuildHints(ctx context.Context, ids []int64) (map[int64][]store.StepHints, error)
	BuildOrder(ctx context.Context, ids []int64) (map[int64][]store.OrderHint, error)
	Ping(ctx context.Context) error
}

// Build is one build in the API.
type Build struct {
	ID        int64             `json:"id"`
	Ref       string            `json:"ref"`
	Daemon    string            `json:"daemon"`
	Target    string            `json:"target"`
	Instance  string            `json:"instance,omitempty"`
	Pipeline  uint64            `json:"pipeline,omitempty"`
	Job       uint64            `json:"job,omitempty"`
	Started   time.Time         `json:"started"`
	Completed *time.Time        `json:"completed,omitempty"`
	Error     string            `json:"error,omitempty"`
	Reason    *Reason           `json:"reason,omitempty"`
	Steps     map[string]int    `json:"steps"`
	Images    []string          `json:"images,omitempty"`
	BuildArgs map[string]string `json:"buildArgs,omitempty"`
	// Hints are the build's cache-mount hints: steps that run a package
	// manager without a cache mount (and ran in the image's builds of the
	// last 30 days), or with one that cannot work.
	Hints []Hint `json:"hints,omitempty"`
	// Order are the steps that installed dependencies again only because
	// a COPY of the whole build context comes before them, in a rebuild
	// GitLab's comparison of the commits proved.
	Order []Order `json:"order,omitempty"`
}

// Order is one step that installed dependencies again only because of
// the Dockerfile's order.
type Order struct {
	Step     string    `json:"step"`
	Name     string    `json:"name"`
	Line     int       `json:"line,omitempty"`
	Copy     string    `json:"copy"`
	CopyLine int       `json:"copyLine,omitempty"`
	Managers []string  `json:"managers"`
	Files    []string  `json:"files"`
	Others   []string  `json:"others,omitempty"`
	Took     float64   `json:"tookSeconds"`
	Fragment string    `json:"fragment,omitempty"`
	Runs     int       `json:"runs"`
	RunTime  float64   `json:"runSeconds"`
	Since    time.Time `json:"since"`
}

// Hint is one step's cache-mount hint.
type Hint struct {
	Step     string       `json:"step"`
	Name     string       `json:"name"`
	Line     int          `json:"line,omitempty"`
	Managers []hints.Hint `json:"managers"`
	Fragment string       `json:"fragment,omitempty"`
	Runs     int          `json:"runs"`
	RunTime  float64      `json:"runSeconds"`
	Median   float64      `json:"medianSeconds"`
	Since    time.Time    `json:"since"`
}

// Reason is why a build's first rebuilt step ran.
type Reason struct {
	Kind  history.ReasonKind `json:"kind"`
	Step  string             `json:"step"`
	Old   string             `json:"old,omitempty"`
	New   string             `json:"new,omitempty"`
	Arg   string             `json:"arg,omitempty"`
	Paths []string           `json:"paths,omitempty"`
	By    string             `json:"by,omitempty"`
}

// Handler returns the API's routes. They log through the role's logger
// when one is given, else through slog's default.
func Handler(s Store, logger ...*slog.Logger) http.Handler {
	log := slog.Default()
	if len(logger) > 0 && logger[0] != nil {
		log = logger[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ping(r.Context()); err != nil {
			// The cause goes to the console's log, not to whoever asks.
			log.Error("health check: store", "err", err)
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/builds", func(w http.ResponseWriter, r *http.Request) {
		// The answer is per person with sign-in (admins only): no cache
		// keeps it, an error answer included.
		w.Header().Set("Cache-Control", "no-store")
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				http.Error(w, "limit: a number from 1 to 1000", http.StatusBadRequest)
				return
			}
			limit = n
		}
		stored, err := s.Recent(r.Context(), limit)
		if err != nil {
			// As for the health check: the cause (SQL, the controller's
			// address) goes to the log.
			log.Error("api: builds", "err", err)
			http.Error(w, "the console's store could not be read", http.StatusInternalServerError)
			return
		}
		ids := make([]int64, 0, len(stored))
		for _, st := range stored {
			ids = append(ids, st.ID)
		}
		// The builds still answer when their hints or order hints cannot
		// be read, each note saying so; the cause goes to the log. Both
		// are read for the newest store.MaxHintBuilds builds only.
		hintsNote, orderNote := "", ""
		if len(ids) > store.MaxHintBuilds {
			hintsNote = fmt.Sprintf("hints for the newest %d builds only", store.MaxHintBuilds)
			orderNote = fmt.Sprintf("order for the newest %d builds only", store.MaxHintBuilds)
		}
		hs, err := s.BuildHints(r.Context(), ids)
		if err != nil {
			log.Error("api: hints", "err", err)
			hs, hintsNote = nil, "unavailable"
		}
		order, err := s.BuildOrder(r.Context(), ids)
		if err != nil {
			log.Error("api: order", "err", err)
			order, orderNote = nil, "unavailable"
		}
		out := make([]Build, 0, len(stored))
		for _, st := range stored {
			v := view(st)
			for _, h := range hs[st.ID] {
				x := Hint{Step: h.Key, Name: h.Name, Managers: managers(h.Hints), Fragment: h.Fragment, Runs: h.Runs,
					RunTime: h.RunTime.Seconds(), Median: h.Median.Seconds(), Since: h.Since}
				if len(h.Lines) > 0 {
					x.Line = h.Lines[0].Start
				}
				v.Hints = append(v.Hints, x)
			}
			for _, o := range order[st.ID] {
				x := Order{Step: o.Key, Name: o.Name, Copy: o.CopyName, Managers: list(o.Managers), Files: list(o.Files), Others: o.Others,
					Took: o.Took.Seconds(), Fragment: o.Fragment, Runs: o.Runs, RunTime: o.RunTime.Seconds(), Since: o.Since}
				if len(o.Lines) > 0 {
					x.Line = o.Lines[0].Start
				}
				if len(o.CopyLines) > 0 {
					x.CopyLine = o.CopyLines[0].Start
				}
				v.Order = append(v.Order, x)
			}
			out = append(out, v)
		}
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"builds": out}
		if hintsNote != "" {
			body["hintsNote"] = hintsNote
		}
		if orderNote != "" {
			body["orderNote"] = orderNote
		}
		json.NewEncoder(w).Encode(body)
	})
	return mux
}

func view(st *store.Stored) Build {
	b := st.Build
	v := Build{
		ID: st.ID, Ref: b.Ref, Daemon: b.Daemon, Target: b.Target, Instance: b.Attribution.Instance,
		Pipeline: b.Attribution.Pipeline, Job: b.Attribution.Job, Started: b.Started, Error: b.Error,
		Steps: map[string]int{}, Images: history.ImageNames(b.Images), BuildArgs: b.BuildArgs,
	}
	if !b.Completed.IsZero() {
		c := b.Completed
		v.Completed = &c
	}
	for _, s := range b.Steps {
		switch {
		case history.Canceled(s.Error):
			v.Steps["canceled"]++
		case s.Error != "":
			v.Steps["failed"]++
		case strings.HasPrefix(s.Name, "FROM "):
			// Resolving a base image: the pages do not count it as a step
			// that ran either.
			v.Steps["image"]++
		default:
			v.Steps[string(s.State)]++
		}
	}
	if r := st.Reason; r.Kind != "" {
		v.Reason = &Reason{Kind: r.Kind, Step: r.Step.Key, Old: r.Old, New: r.New, Arg: r.Arg, Paths: r.Paths, By: r.By}
	}
	return v
}

// list is a list the contract requires: empty, never null (an install of
// named packages reads no project file, for one).
func list(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// managers are a hint's managers with the lists the contract requires
// never null.
func managers(hs []hints.Hint) []hints.Hint {
	out := make([]hints.Hint, 0, len(hs))
	for _, h := range hs {
		if h.Problems == nil {
			h.Problems = []hints.Problem{}
		}
		out = append(out, h)
	}
	return out
}
