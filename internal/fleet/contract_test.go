package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// The management API's answers match docs/api/openapi.yaml, every route
// and every status the test reaches; every route the API serves is in the
// file and every fleet operation of the file is served; every answer is
// marked not to be stored.
func TestAPIMatchesTheContract(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, rt := range (&API{}).routes() {
		served[rt.pattern] = true
	}
	// The boxes' join, beside the API (no API token).
	served["POST "+JoinPath] = true
	documented := map[string]bool{}
	for p, item := range doc.Paths.Map() {
		if !strings.HasPrefix(p, APIPrefix) && p != JoinPath {
			continue
		}
		for m, op := range item.Operations() {
			documented[m+" "+p] = true
			if op.Extensions["x-stability-level"] != "alpha" {
				t.Errorf("%s %s is not marked alpha", m, p)
			}
			if !served[m+" "+p] {
				t.Errorf("%s %s is in the contract, the API does not serve it", m, p)
			}
		}
	}
	for p := range served {
		if !documented[p] {
			t.Errorf("%s is served, the contract has no such operation", p)
		}
	}
	doc.Servers = nil
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := openReg(t)
	if err := r.SyncOwners(ctx, []string{"gitlab:gitlab.example.com/7"}); err != nil {
		t.Fatal(err)
	}
	sealer, _ := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	a := &API{Registry: r, Version: "test", Who: testWho, Sealer: sealer,
		Status: func(string) Status { return Status{Connected: true, Protocol: 1} }}
	mux := http.NewServeMux()
	a.Register(mux)
	mux.Handle("POST "+JoinPath, r.JoinHandler())
	_, pub, _ := GenerateKey()
	_, joinPub, _ := GenerateKey()
	_, joinTok, err := r.IssueJoin(ctx, "tester", "jn", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := r.IssueJoin(ctx, "tester", "jx", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	joinBody := func(name, token string) string {
		b, _ := json.Marshal(map[string]string{"name": name, "token": token, "public_key": string(joinPub)})
		return string(b)
	}
	add, _ := json.Marshal(map[string]string{"name": "eu", "public_key": string(pub)})
	big := `{"name":"eu","public_key":"` + strings.Repeat("x", 100<<10) + `"}`
	seen := map[string]bool{}
	for _, c := range []struct {
		method, path, token, body string
		want                      int
	}{
		{"GET", "/api/v1/fleet/version", "good", "", 200},
		{"GET", "/api/v1/fleet/members", "", "", 401},
		{"POST", "/api/v1/fleet/members", "good", string(add), 201},
		{"POST", "/api/v1/fleet/members", "good", string(add), 409},
		{"POST", "/api/v1/fleet/members", "good", `{"name":"Bad","public_key":"x"}`, 400},
		{"GET", "/api/v1/fleet/members", "good", "", 200},
		{"GET", "/api/v1/fleet/members/eu", "good", "", 200},
		{"GET", "/api/v1/fleet/members/none", "good", "", 404},
		{"POST", "/api/v1/fleet/members", "good", big, 413},
		{"GET", "/api/v1/fleet/members/eu/values", "good", "", 404},
		{"DELETE", "/api/v1/fleet/members/eu", "good", "", 204},
		{"GET", "/api/v1/fleet/members/eu", "good", "", 200},
		{"GET", "/api/v1/fleet/members", "good", "", 200},
		{"GET", "/api/v1/fleet/members", "view", "", 403},
		{"POST", "/api/v1/fleet/members/ap/join-tokens", "op", `{"expires_in_minutes":120}`, 201},
		{"POST", "/api/v1/fleet/members/ap/join-tokens", "op", "", 201},
		{"POST", "/api/v1/fleet/members/ap/join-tokens", "op", `{"expires_in_minutes":99999}`, 400},
		{"POST", "/api/v1/fleet/members/ap/join-tokens", "view", "", 403},
		{"GET", "/api/v1/fleet/members/ap/join-tokens", "op", "", 200},
		{"GET", "/api/v1/fleet/members/ap/join-tokens/4", "op", "", 200},
		{"GET", "/api/v1/fleet/members/ap/join-tokens/99", "op", "", 404},
		{"GET", "/api/v1/fleet/members/ap/join-tokens/x", "op", "", 400},
		{"GET", "/api/v1/fleet/members/ap/settings", "op", "", 404},
		{"PUT", "/api/v1/fleet/members/ap/settings", "op", `{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir","runner_token":"glrt-not-a-real-token","gitlab_token":"glpat-read"}}`, 200},
		{"PUT", "/api/v1/fleet/members/ap/settings", "op", `{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir"},"forget":["gitlab_token"]}`, 200},
		{"PUT", "/api/v1/fleet/members/ap/settings", "op", `{"values":{"gitlab_url":"nope"}}`, 400},
		{"PUT", "/api/v1/fleet/members/ap/settings", "view", `{"values":{}}`, 403},
		{"GET", "/api/v1/fleet/members/ap/settings", "op", "", 200},
		{"GET", "/api/v1/fleet/members/ap/values", "op", "", 200},
		{"GET", "/api/v1/fleet/members/none/values", "op", "", 404},
		{"DELETE", "/api/v1/fleet/members/ap/join-tokens/4", "op", "", 204},
		{"DELETE", "/api/v1/fleet/members/ap/join-tokens/x", "op", "", 400},
		{"GET", "/api/v1/fleet/audit?limit=10", "good", "", 200},
		{"GET", "/api/v1/fleet/audit?limit=10&actor=token:t&before=99", "good", "", 200},
		{"GET", "/api/v1/fleet/audit?limit=0", "good", "", 400},
		{"PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "good", `{"role":"operator"}`, 200},
		{"PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "good", `{"role":"boss"}`, 400},
		{"PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/7", "good", `{"role":"owner"}`, 409},
		{"PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "op", `{"role":"owner"}`, 403},
		{"GET", "/api/v1/fleet/role-bindings", "op", "", 200},
		{"GET", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "op", "", 200},
		{"PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "good", `{"role":"viewer"}`, 204},
		{"GET", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "op", "", 404},
		{"GET", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/43", "op", "", 404},
		{"GET", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/0", "op", "", 400},
		{"DELETE", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "good", "", 204},
		{"DELETE", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/0", "good", "", 400},
		{"POST", "/api/v1/fleet/tokens", "good", `{"name":"terraform","role":"owner","expires_in_days":30}`, 201},
		{"POST", "/api/v1/fleet/tokens", "good", `{"name":"terraform","role":"owner"}`, 409},
		{"POST", "/api/v1/fleet/tokens", "good", `{"name":"bootstrap","role":"owner"}`, 400},
		{"GET", "/api/v1/fleet/tokens", "op", "", 403},
		{"GET", "/api/v1/fleet/tokens", "good", "", 200},
		{"GET", "/api/v1/fleet/tokens/terraform", "good", "", 200},
		{"GET", "/api/v1/fleet/tokens/none", "good", "", 404},
		{"DELETE", "/api/v1/fleet/tokens/terraform", "good", "", 204},
		{"DELETE", "/api/v1/fleet/tokens/bootstrap", "good", "", 409},
		{"POST", JoinPath, "", "not json", 400},
		{"POST", JoinPath, "", joinBody("jn", "wrong"), 403},
		{"POST", JoinPath, "", joinBody("jn", joinTok), 200},
		{"POST", JoinPath, "", joinBody("jx", otherTok), 409},
	} {
		req := httptest.NewRequest(c.method, "http://localhost:8080"+c.path, strings.NewReader(c.body))
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s %s: %d, want %d: %.300s", c.method, c.path, rec.Code, c.want, rec.Body)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s (%d): Cache-Control %q", c.method, c.path, rec.Code, rec.Header().Get("Cache-Control"))
		}
		route, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("%s %s: not in the contract: %v", c.method, c.path, err)
		}
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}},
			Status: rec.Code, Header: rec.Header(), Body: io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		if err := openapi3filter.ValidateResponse(ctx, in); err != nil {
			t.Errorf("%s %s (%d): %v", c.method, c.path, rec.Code, err)
		}
		seen[c.method+" "+route.Path] = true
	}
	for op := range documented {
		if !seen[op] {
			t.Errorf("%s: no answer checked", op)
		}
	}
}
