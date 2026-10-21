// Package api serves the console's read-only JSON API under /api/v1 and
// its health check. It has no authentication of its own yet: until sign-in
// through GitLab exists it must not be exposed beyond the cluster.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Store is what the API reads.
type Store interface {
	Recent(ctx context.Context, limit int) ([]*store.Stored, error)
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

// Handler returns the API's routes.
func Handler(s Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ping(r.Context()); err != nil {
			http.Error(w, "store: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/builds", func(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]Build, 0, len(stored))
		for _, st := range stored {
			out = append(out, view(st))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"builds": out})
	})
	return mux
}

func view(st *store.Stored) Build {
	b := st.Build
	v := Build{
		ID: st.ID, Ref: b.Ref, Daemon: b.Daemon, Target: b.Target, Instance: b.Attribution.Instance,
		Pipeline: b.Attribution.Pipeline, Job: b.Attribution.Job, Started: b.Started, Error: b.Error,
		Steps: map[string]int{}, Images: b.Images, BuildArgs: b.BuildArgs,
	}
	if !b.Completed.IsZero() {
		c := b.Completed
		v.Completed = &c
	}
	for _, s := range b.Steps {
		if s.Error != "" {
			v.Steps["failed"]++
			continue
		}
		v.Steps[string(s.State)]++
	}
	if r := st.Reason; r.Kind != "" {
		v.Reason = &Reason{Kind: r.Kind, Step: r.Step.Key, Old: r.Old, New: r.New, Arg: r.Arg, Paths: r.Paths, By: r.By}
	}
	return v
}
