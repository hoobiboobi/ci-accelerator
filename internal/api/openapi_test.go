package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

const contract = "../../docs/api/openapi.yaml"

// The responses are what docs/api/openapi.yaml says, field by field: every
// object there refuses properties it does not list, so a field added here
// without the contract fails this test, and every field of the contract
// must show up in the responses below (a contract this test never fills
// would prove nothing).
func TestResponsesMatchTheContract(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatalf("%s is not a valid OpenAPI document: %v", contract, err)
	}
	path := "/api/v1/builds"
	item := doc.Paths.Find(path)
	if item == nil || item.Get == nil {
		t.Fatalf("%s has no GET %s", contract, path)
	}
	route := &routers.Route{Spec: doc, Path: path, PathItem: item, Method: http.MethodGet, Operation: item.Get}

	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	full := &history.Build{Ref: "r1", Daemon: "d1", Target: "api", Started: t0, Completed: t0.Add(time.Minute),
		Error: "process did not complete", Images: []string{"registry.example.com/app:1"}, BuildArgs: map[string]string{"VERSION": "2"},
		Attribution: history.Attribution{State: history.Attributed, Instance: "abcdef", Pipeline: 7, Job: 70},
		Steps: []history.Step{{Key: "[a 1/2]", Name: "FROM alpine:3.22", State: history.Built},
			{Key: "[a 2/2]", Name: "RUN apk add jq", State: history.Built, Lines: []history.LineRange{{Start: 3, End: 3}}}}}
	running := &history.Build{Ref: "r2", Daemon: "d1", Started: t0, Steps: []history.Step{{Key: "[1/1]", State: history.Cached}}}
	reason := history.Reason{Kind: history.ArgChanged, Step: full.Steps[1], Old: "1", New: "2", Arg: "VERSION",
		Paths: []string{"app/"}, By: "r0"}
	hint := store.StepHints{Step: 1, Key: "[a 2/2]", Name: "RUN apk add jq", Lines: []history.LineRange{{Start: 3, End: 3}},
		Fragment: "RUN --mount=type=cache,target=/etc/apk/cache,sharing=locked apk add jq", Runs: 3,
		RunTime: 6 * time.Second, Median: 2 * time.Second, Since: t0.Add(-30 * 24 * time.Hour),
		Hints: []hints.Hint{{Manager: "apk", PathCommand: "apk --print-arch", Owner: "uid=1000,gid=1000",
			Problems: []hints.Problem{{Kind: hints.Missing, Mount: "/etc/apk/cache", Setting: "--no-cache", Cache: "packages"}},
			Caches:   []hints.Cache{{Label: "packages", Path: "/etc/apk/cache", Locked: true, ID: "apk"}},
			Before:   []string{"RUN rm -f /etc/apk/cache"}, Drop: []string{"--no-cache"},
			Insert: []hints.Insert{{After: "apk", Add: "--cache-dir /etc/apk/cache"}}, Caveats: []string{"shared by every project"}},
			// A manager whose cache could not be placed has no folders.
			{Manager: "npm", Problems: []hints.Problem{{Kind: hints.Missing}}}}}
	f := &fakeStore{builds: []*store.Stored{{ID: 1, Build: full, Reason: reason}, {ID: 2, Build: running}},
		hints: map[int64][]store.StepHints{1: {hint}}}
	h := Handler(f)

	check := func(name, query string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d", name, w.Code, want)
		}
		body := w.Body.Bytes()
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, Route: route},
			Status:                 w.Code, Header: w.Header(), Body: io.NopCloser(strings.NewReader(string(body))),
			Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
		}
		if err := openapi3filter.ValidateResponse(ctx, in); err != nil {
			t.Errorf("%s: the response is not what %s says: %v", name, contract, err)
		}
		return body
	}
	body := check("builds", "?limit=10", http.StatusOK)
	f.hintsErr = errors.New("store closed")
	unavailable := check("builds without hints", "", http.StatusOK)
	f.hintsErr = nil
	check("a limit out of range", "?limit=0", http.StatusBadRequest)

	// Every field of the contract appears in the responses above.
	seen := map[string]bool{}
	for _, b := range [][]byte{body, unavailable} {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		collect(v, "", seen)
	}
	var missing []string
	for _, p := range contractFields(doc.Components.Schemas["BuildList"].Value, "") {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fields of the contract no response above fills (extend the test): %v", missing)
	}
}

// collect records every object key of a JSON value by its path, with
// arrays written as [] and maps of arbitrary keys left out.
func collect(v any, prefix string, seen map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := prefix + "." + k
			seen[p] = true
			if strings.HasSuffix(p, ".steps") || strings.HasSuffix(p, ".buildArgs") {
				continue
			}
			collect(e, p, seen)
		}
	case []any:
		for _, e := range x {
			collect(e, prefix+"[]", seen)
		}
	}
}

// contractFields lists every property path of a schema, as collect writes
// them.
func contractFields(s *openapi3.Schema, prefix string) []string {
	var out []string
	for name, ref := range s.Properties {
		p := prefix + "." + name
		out = append(out, p)
		v := ref.Value
		if v.Type.Is("array") && v.Items != nil {
			out = append(out, contractFields(v.Items.Value, p+"[]")...)
		} else if len(v.Properties) > 0 {
			out = append(out, contractFields(v, p)...)
		}
	}
	return out
}
