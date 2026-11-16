package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// againstContract answers GET /api/v1/builds<query> and checks the answer
// against the contract as published (no strictness added).
func againstContract(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/builds"
	item := doc.Paths.Find(path)
	route := &routers.Route{Spec: doc, Path: path, PathItem: item, Method: http.MethodGet, Operation: item.Get}
	req := httptest.NewRequest(http.MethodGet, path+query, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, Route: route},
		Status:                 w.Code, Header: w.Header(), Body: io.NopCloser(strings.NewReader(w.Body.String())),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if err := openapi3filter.ValidateResponse(ctx, in); err != nil {
		t.Errorf("the response is not what %s says: %v", contract, err)
	}
	return w
}

// An install of named packages reads no project file: its order hint has
// no files, and the contract's required lists are empty, never null.
func TestRequiredListsAreNeverNull(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	b := &history.Build{Ref: "r1", Daemon: "d1", Started: t0, Steps: []history.Step{{Key: "[2/2]", Name: "RUN pip install requests flask", State: history.Built}}}
	f := &fakeStore{builds: []*store.Stored{{ID: 1, Build: b}},
		order: map[int64][]store.OrderHint{1: {{Key: "[2/2]", Name: "RUN pip install requests flask", CopyName: "COPY . .", Took: time.Second, Since: t0}}},
		hints: map[int64][]store.StepHints{1: {{Key: "[2/2]", Name: "RUN pip install requests flask", Since: t0,
			Hints: []hints.Hint{{Manager: "pip"}}}}}}
	w := againstContract(t, Handler(f), "")
	body := w.Body.String()
	for _, want := range []string{`"files":[]`, `"managers":[]`, `"problems":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in %s", want, body)
		}
	}
	// A hint without managers answers [] too.
	f.hints = map[int64][]store.StepHints{1: {{Key: "[2/2]", Name: "RUN pip install requests flask", Since: t0}}}
	againstContract(t, Handler(f), "")
}

// The CI component pushes a commit's tag and latest in one exporter
// attribute: the API lists one image name per item, as the build page.
func TestImagesOneNamePerItem(t *testing.T) {
	b := &history.Build{Ref: "r1", Daemon: "d1", Started: time.Now(),
		Images: []string{"reg.example.com/g/app:abc,reg.example.com/g/app:latest", "reg.example.com/g/other:1"}}
	var out struct{ Builds []Build }
	if err := json.Unmarshal(get(t, Handler(&fakeStore{builds: []*store.Stored{{ID: 1, Build: b}}}), "/api/v1/builds").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"reg.example.com/g/app:abc", "reg.example.com/g/app:latest", "reg.example.com/g/other:1"}
	if got := out.Builds[0].Images; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("images %q, want %q", got, want)
	}
}

type failingStore struct{ fakeStore }

func (f *failingStore) Recent(context.Context, int) ([]*store.Stored, error) {
	return nil, errors.New(`Post "http://10.0.0.7:8081/internal/v1/call/Recent": dial tcp: connection refused`)
}

// No cache keeps the API's answers (admins' data with sign-in), and a
// failing store's cause stays in the log.
func TestNoStoreAndNoErrorDetail(t *testing.T) {
	ok := &fakeStore{builds: []*store.Stored{{ID: 1, Build: &history.Build{Ref: "r1", Started: time.Now()}}}}
	for _, c := range []struct {
		name  string
		h     http.Handler
		query string
		code  int
	}{{"builds", Handler(ok), "", 200}, {"bad limit", Handler(ok), "?limit=x", 400}, {"store down", Handler(&failingStore{}), "", 500}} {
		w := get(t, c.h, "/api/v1/builds"+c.query)
		if w.Code != c.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: status %d, Cache-Control %q", c.name, w.Code, w.Header().Get("Cache-Control"))
		}
		if c.code == 500 && (strings.Contains(w.Body.String(), "10.0.0.7") || strings.Contains(w.Body.String(), "dial")) {
			t.Errorf("%s: the cause reached the caller: %q", c.name, w.Body.String())
		}
	}
	againstContract(t, Handler(&failingStore{}), "")
}

// The order block's figures and the two notes: each note speaks of its
// own list only.
func TestOrderValuesAndNotes(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	b := &history.Build{Ref: "r1", Daemon: "d1", Started: t0}
	o := store.OrderHint{Key: "[3/4]", Name: "RUN pip install .", CopyName: "COPY . .", Managers: []string{"pip"}, Files: []string{"pyproject.toml"},
		Lines: []history.LineRange{{Start: 5, End: 6}}, CopyLines: []history.LineRange{{Start: 3, End: 4}},
		Took: 2 * time.Second, Runs: 4, RunTime: 90 * time.Second, Since: t0}
	h := store.StepHints{Key: "[2/4]", Name: "RUN npm ci", Hints: []hints.Hint{{Manager: "npm", Problems: []hints.Problem{{Kind: hints.Missing}}}}, Since: t0}
	f := &fakeStore{builds: []*store.Stored{{ID: 1, Build: b}}, order: map[int64][]store.OrderHint{1: {o}}, hints: map[int64][]store.StepHints{1: {h}}}
	type answer struct {
		Builds    []Build
		HintsNote string
		OrderNote string
	}
	read := func() answer {
		t.Helper()
		var a answer
		if err := json.Unmarshal(againstContract(t, Handler(f), "?limit=1000").Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := read()
	x := a.Builds[0].Order[0]
	if x.Took != 2 || x.Line != 5 || x.CopyLine != 3 || x.Runs != 4 || x.RunTime != 90 || a.HintsNote != "" || a.OrderNote != "" {
		t.Errorf("order %+v, notes %q %q", x, a.HintsNote, a.OrderNote)
	}
	// The order hints cannot be read: the hints are there and their note
	// says nothing; the order's note says so.
	f.orderErr = errors.New("store down")
	if a = read(); len(a.Builds[0].Hints) != 1 || a.HintsNote != "" || a.OrderNote != "unavailable" || len(a.Builds[0].Order) != 0 {
		t.Errorf("order unreadable: hints %d, notes %q %q", len(a.Builds[0].Hints), a.HintsNote, a.OrderNote)
	}
	f.orderErr, f.hintsErr = nil, errors.New("store down")
	if a = read(); len(a.Builds[0].Order) != 1 || a.HintsNote != "unavailable" || a.OrderNote != "" {
		t.Errorf("hints unreadable: order %d, notes %q %q", len(a.Builds[0].Order), a.HintsNote, a.OrderNote)
	}
	// More builds than both are read for: both notes say so.
	f.hintsErr = nil
	for i := int64(2); i <= store.MaxHintBuilds+1; i++ {
		f.builds = append(f.builds, &store.Stored{ID: i, Build: b})
	}
	if a = read(); !strings.Contains(a.HintsNote, "newest 100") || !strings.Contains(a.OrderNote, "newest 100") {
		t.Errorf("over %d builds: notes %q %q", store.MaxHintBuilds, a.HintsNote, a.OrderNote)
	}
}

// The published schemas let new fields through, as the contract promises
// (internal/api's contract test adds the strictness for itself).
func TestPublishedSchemasAllowNewFields(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	walk(doc.Components.Schemas["BuildList"].Value, "BuildList", map[*openapi3.Schema]bool{}, func(s *openapi3.Schema, at string) {
		if s.AdditionalProperties.Has != nil && !*s.AdditionalProperties.Has {
			t.Errorf("%s: additionalProperties: false refuses the fields a later v1 may add", at)
		}
	})
}

// walk calls fn for s and every object schema below it.
func walk(s *openapi3.Schema, at string, seen map[*openapi3.Schema]bool, fn func(*openapi3.Schema, string)) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	fn(s, at)
	for name, p := range s.Properties {
		walk(p.Value, at+"."+name, seen, fn)
	}
	if s.Items != nil {
		walk(s.Items.Value, at+"[]", seen, fn)
	}
	if s.AdditionalProperties.Schema != nil {
		walk(s.AdditionalProperties.Schema.Value, at+"{}", seen, fn)
	}
}

// A required list that may become null breaks a typed client, and the
// breaking-change check does not see a property inside an array's items
// become nullable: the nullable properties are pinned here.
func TestNullablePropertiesStay(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	walk(doc.Components.Schemas["BuildList"].Value, "BuildList", map[*openapi3.Schema]bool{}, func(s *openapi3.Schema, at string) {
		if s.Nullable {
			got = append(got, at)
		}
	})
	if want := "BuildList.builds[].hints[].managers[].caches"; strings.Join(got, " ") != want {
		t.Errorf("nullable: %q, want only %s (a client of an earlier release takes the others as never null)", got, want)
	}
}
