package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecretsKey = "0123456789abcdef0123456789abcdef"

// apiCall is one request to the API on mux: status, headers and body.
type apiCall struct {
	code   int
	header http.Header
	body   string
}

func callAPI(t *testing.T, mux http.Handler, method, path, token, body string) apiCall {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	req := httptest.NewRequest(method, "http://localhost:8080"+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return apiCall{rec.Code, rec.Header(), rec.Body.String()}
}

func testAPI(t *testing.T, sealer *Sealer) (*Registry, *API, *http.ServeMux) {
	t.Helper()
	r, _ := openReg(t)
	if err := r.SyncOwners(context.Background(), []string{"gitlab:gitlab.example.com/7"}); err != nil {
		t.Fatal(err)
	}
	a := &API{Registry: r, Version: "0.8.0", Who: testWho, Sealer: sealer,
		Hub:    HubInfo{URL: "https://hub.example.com", PublicKeys: "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAhub\n-----END PUBLIC KEY-----\n"},
		Status: func(string) Status { return Status{} }}
	mux := http.NewServeMux()
	a.Register(mux)
	return r, a, mux
}

func pubKey(t *testing.T) string {
	t.Helper()
	_, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return string(pub)
}

func addBody(name, pub string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "public_key": pub})
	return string(b)
}

// Removing a cluster through the API forgets what the hub kept for it, as
// the console's removal does: a key registered later under the name gets
// none of the old cluster's secrets, and its unused join tokens end.
func TestAPIRemoveForgetsTheCluster(t *testing.T) {
	ctx := context.Background()
	sealer, _ := NewSealer([]byte(testSecretsKey))
	r, _, mux := testAPI(t, sealer)
	// An owner keeps the runner token of a registered cluster.
	if c := callAPI(t, mux, "POST", "/api/v1/fleet/members", "good", addBody("eu", pubKey(t))); c.code != 201 {
		t.Fatalf("add: %d %s", c.code, c.body)
	}
	put := `{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir","runner_token":"glrt-not-a-real-token"}}`
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good", put); c.code != 200 {
		t.Fatalf("settings: %d %s", c.code, c.body)
	}
	// An operator removes it and registers the name with a key of their own.
	if c := callAPI(t, mux, "DELETE", "/api/v1/fleet/members/eu", "op", ""); c.code != 204 {
		t.Fatalf("remove: %d %s", c.code, c.body)
	}
	if _, err := r.Settings(ctx, "eu"); !errors.Is(err, ErrNotFound) {
		t.Errorf("settings kept after the removal: %v", err)
	}
	if c := callAPI(t, mux, "POST", "/api/v1/fleet/members", "op", addBody("eu", pubKey(t))); c.code != 201 {
		t.Fatalf("add again: %d %s", c.code, c.body)
	}
	if d, err := r.SecretsFor(ctx, sealer, "eu"); err != nil || len(d) != 0 {
		t.Errorf("the new key under the name would get: %v %v", d, err)
	}
	// A cluster waiting to join: its join token and settings go too.
	if c := callAPI(t, mux, "POST", "/api/v1/fleet/members/us/join-tokens", "op", ""); c.code != 201 {
		t.Fatalf("join token: %d %s", c.code, c.body)
	}
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/us/settings", "good", put); c.code != 200 {
		t.Fatalf("settings: %d %s", c.code, c.body)
	}
	if c := callAPI(t, mux, "DELETE", "/api/v1/fleet/members/us", "op", ""); c.code != 204 {
		t.Fatalf("remove pending: %d %s", c.code, c.body)
	}
	js, _ := r.JoinTokens(ctx, "us")
	for _, j := range js {
		if j.RevokedAt.IsZero() {
			t.Errorf("join token #%d still active after the removal", j.ID)
		}
	}
	if _, err := r.Settings(ctx, "us"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a pending cluster's settings kept after the removal: %v", err)
	}
	es, _ := r.AuditQuery(ctx, AuditQuery{Target: "us"})
	var acts []string
	for _, e := range es {
		acts = append(acts, e.Action)
	}
	if got := strings.Join(acts, " "); !strings.Contains(got, "settings.delete") || !strings.Contains(got, "join-token.revoke") {
		t.Errorf("audit of the removal: %s", got)
	}
}

// A member registered by its key through the API gets values that name
// the key's Secret, also when its settings say nothing of "joins".
func TestValuesOfAMemberRegisteredByKey(t *testing.T) {
	sealer, _ := NewSealer([]byte(testSecretsKey))
	_, _, mux := testAPI(t, sealer)
	pub := pubKey(t)
	callAPI(t, mux, "POST", "/api/v1/fleet/members", "good", addBody("eu", pub))
	put := `{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir","runner_token":"glrt-not-a-real-token"}}`
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good", put); c.code != 200 {
		t.Fatalf("settings: %d %s", c.code, c.body)
	}
	c := callAPI(t, mux, "GET", "/api/v1/fleet/members/eu/values", "good", "")
	if c.code != 200 || !strings.Contains(c.body, "key:\n      existingSecret: \"fleet-key\"") || strings.Contains(c.body, "fleet-join") {
		t.Errorf("values of a member registered by key: %d\n%s", c.code, c.body)
	}
	// Settings naming another key than the registered one are refused.
	other, _ := json.Marshal(map[string]any{"values": map[string]string{"joins": "key", "public_key": pubKey(t),
		"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir"}})
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good", string(other)); c.code != 400 || !strings.Contains(c.body, "public_key") {
		t.Errorf("another key in the settings: %d %s", c.code, c.body)
	}
}

// Generated commands: the published chart, and commands that can run
// again (a re-issued join token replaces the Secret).
func TestGeneratedCommands(t *testing.T) {
	s := Settings{Member: "eu", Values: map[string]string{"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir"}}
	in := Generate(s, HubInfo{URL: "https://hub.example.com"}, "0.8.0", "cia_join_SECRET", false)
	cmds := strings.Join(in.Commands, "\n")
	for _, want := range []string{
		"helm upgrade --install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.8.0",
		"kubectl create namespace ci-accelerator --dry-run=client -o yaml | kubectl apply -f -",
		"create secret generic fleet-join --from-literal=token=cia_join_SECRET --dry-run=client -o yaml | kubectl apply -f -",
		"docs/install.md",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("commands: no %q in\n%s", want, cmds)
		}
	}
	// A cluster with neither S3 nor a GitLab token: no Secret of them.
	if strings.Contains(in.Values, SecretS3) || strings.Contains(in.Values, SecretGitLab) {
		t.Errorf("values name a Secret nothing delivers:\n%s", in.Values)
	}
	// Joining by key: the key's Secret, the private CA.
	s.Values["joins"] = "key"
	k := Generate(s, HubInfo{URL: "https://hub.example.com", CACert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}, "0.8.0", "", false)
	cmds = strings.Join(k.Commands, "\n")
	if !strings.Contains(cmds, "create secret generic fleet-key --from-file=key=./eu.key --dry-run=client") ||
		!strings.Contains(k.Values, "existingSecret: \"fleet-key\"") || !strings.Contains(k.Values, "caCert: |\n        -----BEGIN CERTIFICATE-----") {
		t.Errorf("joining by key:\n%s\n%s", cmds, k.Values)
	}
}

// What the values file says is what a YAML reader reads: no character a
// reader folds, no block it cannot parse.
func TestValuesQuoting(t *testing.T) {
	s := Settings{Member: "eu", Values: map[string]string{}}
	in := Generate(s, HubInfo{URL: "https://hub.example.com/\u0085image: evil", CACert: "  # indented\n-----BEGIN CERTIFICATE-----\n"}, "0.8.0", "", true)
	if strings.Contains(in.Values, "\u0085") || !strings.Contains(in.Values, `\u0085`) {
		t.Errorf("U+0085 written as it is:\n%q", in.Values)
	}
	if strings.Contains(in.Values, "caCert: |") || !strings.Contains(in.Values, `caCert: "  # indented\n-----BEGIN CERTIFICATE-----\n"`) {
		t.Errorf("an indented first line in a literal block:\n%s", in.Values)
	}
	for _, c := range []struct{ key, value string }{
		{"gitlab_url", "https://gitlab.example.com/\u0085x"},
		{"gitlab_url", "https://user:pass@gitlab.example.com/"},
		{"host_path", "/.."},
		{"host_path", "/var//lib"},
		{"host_path", "/var/lib/../../etc"},
	} {
		bad := CheckSettings(map[string]string{"gitlab_url": "https://gitlab.example.com/", c.key: c.value}, map[string]bool{"runner_token": true})
		if len(bad) != 1 || bad[0].Key != c.key {
			t.Errorf("%s=%q: %v", c.key, c.value, bad)
		}
	}
}

// The box's chart needs a region with an S3 server; a hub without a
// secrets key asks for no secret; secrets given for S3 without a server
// are refused rather than dropped.
func TestSettingsChecks(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	sealer, _ := NewSealer([]byte(testSecretsKey))
	_, err := r.SaveSettings(ctx, sealer, "x", "eu", map[string]string{"gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-not-a-real-token",
		"s3_server": "minio.example.com", "s3_bucket": "ci-cache", "s3_access_key": "a", "s3_secret_key": "b"})
	var se *SettingsError
	if !errors.As(err, &se) || len(se.Fields) != 1 || se.Fields[0].Key != "s3_region" {
		t.Errorf("S3 without a region: %v", err)
	}
	if _, err := r.SaveSettings(ctx, nil, "x", "eu", map[string]string{"gitlab_url": "https://gitlab.example.com/", "s3_server": "minio.example.com",
		"s3_bucket": "ci-cache", "s3_region": "us-east-1"}); err != nil {
		t.Errorf("settings on a hub without a secrets key: %v", err)
	}
	_, err = r.SaveSettings(ctx, sealer, "x", "us", map[string]string{"gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-not-a-real-token",
		"s3_access_key": "a", "s3_secret_key": "b"})
	if !errors.As(err, &se) || len(se.Fields) != 2 || se.Fields[0].Key != "s3_access_key" {
		t.Errorf("S3 keys without a server: %v", err)
	}
}

// Saving the same settings again delivers nothing again and audits
// nothing.
func TestSettingsSavedAgainUnchanged(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	sealer, _ := NewSealer([]byte(testSecretsKey))
	vals := map[string]string{"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir", "runner_token": "glrt-not-a-real-token"}
	if _, err := r.SaveSettings(ctx, sealer, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	if err := r.Delivered(ctx, "eu", []string{SecretRunner}); err != nil {
		t.Fatal(err)
	}
	before, _ := r.AuditQuery(ctx, AuditQuery{Target: "eu"})
	if _, err := r.SaveSettings(ctx, sealer, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.Undelivered(ctx, "eu"); u {
		t.Error("the same secret again counts as undelivered")
	}
	if after, _ := r.AuditQuery(ctx, AuditQuery{Target: "eu"}); len(after) != len(before) {
		t.Errorf("the same settings again audited: %d -> %d records", len(before), len(after))
	}
}

// After the secrets key changed, a required secret sealed with the old
// one must be given again, and the settings say which ones do not open.
func TestSecretsKeyChanged(t *testing.T) {
	ctx := context.Background()
	old, _ := NewSealer([]byte(testSecretsKey))
	r, a, mux := testAPI(t, old)
	vals := map[string]string{"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir", "runner_token": "glrt-not-a-real-token", "gitlab_token": "glpat-read"}
	if _, err := r.SaveSettings(ctx, old, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	a.Sealer, _ = NewSealer([]byte("another-key-another-key-another-key"))
	delete(vals, "runner_token")
	delete(vals, "gitlab_token")
	_, err := r.SaveSettings(ctx, a.Sealer, "x", "eu", vals)
	var se *SettingsError
	if !errors.As(err, &se) || se.Fields[0].Key != "runner_token" {
		t.Errorf("a runner token that does not open counted as kept: %v", err)
	}
	c := callAPI(t, mux, "GET", "/api/v1/fleet/members/eu/settings", "good", "")
	if c.code != 200 || strings.Count(c.body, `"stale":true`) != 2 {
		t.Errorf("stale secrets not shown: %s", c.body)
	}
}

// A kept optional secret can be forgotten through the API.
func TestForgetASecret(t *testing.T) {
	sealer, _ := NewSealer([]byte(testSecretsKey))
	_, _, mux := testAPI(t, sealer)
	put := `{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir","runner_token":"glrt-not-a-real-token","gitlab_token":"glpat-read"}}`
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good", put); c.code != 200 {
		t.Fatalf("settings: %d %s", c.code, c.body)
	}
	c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good",
		`{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir"},"forget":["gitlab_token"]}`)
	if c.code != 200 || strings.Contains(c.body, "gitlab_token") || !strings.Contains(c.body, "runner_token") {
		t.Errorf("forget: %d %s", c.code, c.body)
	}
	if c := callAPI(t, mux, "GET", "/api/v1/fleet/members/eu/values", "good", ""); strings.Contains(c.body, SecretGitLab) {
		t.Errorf("values name a forgotten secret:\n%s", c.body)
	}
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/members/eu/settings", "good",
		`{"values":{"gitlab_url":"https://gitlab.example.com/","storage":"emptyDir"},"forget":["runner_token"]}`); c.code != 400 {
		t.Errorf("forgetting a required secret: %d %s", c.code, c.body)
	}
}

// Every route of the API needs its role: nobody gets 401, a role without
// the permission 403. The roles are written out here, not taken from the
// routes, so that a weakened route turns this test red.
func TestAPIPermissions(t *testing.T) {
	sealer, _ := NewSealer([]byte(testSecretsKey))
	_, _, mux := testAPI(t, sealer)
	least := map[string]string{
		"GET /api/v1/fleet/version":                             "view",
		"GET /api/v1/fleet/members":                             "op",
		"POST /api/v1/fleet/members":                            "op",
		"GET /api/v1/fleet/members/eu":                          "op",
		"DELETE /api/v1/fleet/members/eu":                       "op",
		"GET /api/v1/fleet/members/eu/join-tokens":              "op",
		"POST /api/v1/fleet/members/eu/join-tokens":             "op",
		"GET /api/v1/fleet/members/eu/join-tokens/1":            "op",
		"DELETE /api/v1/fleet/members/eu/join-tokens/1":         "op",
		"GET /api/v1/fleet/members/eu/settings":                 "op",
		"PUT /api/v1/fleet/members/eu/settings":                 "op",
		"GET /api/v1/fleet/members/eu/values":                   "op",
		"GET /api/v1/fleet/role-bindings":                       "op",
		"GET /api/v1/fleet/role-bindings/gitlab/g.example/5":    "op",
		"PUT /api/v1/fleet/role-bindings/gitlab/g.example/5":    "good",
		"DELETE /api/v1/fleet/role-bindings/gitlab/g.example/5": "good",
		"GET /api/v1/fleet/tokens":                              "good",
		"POST /api/v1/fleet/tokens":                             "good",
		"GET /api/v1/fleet/tokens/x":                            "good",
		"DELETE /api/v1/fleet/tokens/x":                         "good",
		"GET /api/v1/fleet/audit":                               "op",
	}
	rank := map[string]int{"view": 0, "op": 1, "good": 2}
	for call, min := range least {
		method, path, _ := strings.Cut(call, " ")
		if c := callAPI(t, mux, method, path, "", "{}"); c.code != 401 || !strings.HasPrefix(c.header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s by nobody: %d %v", call, c.code, c.header)
		}
		for who, rk := range rank {
			c := callAPI(t, mux, method, path, who, "{}")
			if forbidden := c.code == 403; forbidden != (rk < rank[min]) {
				t.Errorf("%s by %s: %d %s", call, who, c.code, c.body)
			}
		}
	}
}

// Answers of the API that a client can rely on.
func TestAPIAnswers(t *testing.T) {
	sealer, _ := NewSealer([]byte(testSecretsKey))
	_, _, mux := testAPI(t, sealer)
	// Version: only protocols the hub accepts.
	c := callAPI(t, mux, "GET", "/api/v1/fleet/version", "view", "")
	var v struct{ Protocols []int }
	json.Unmarshal([]byte(c.body), &v)
	for _, p := range v.Protocols {
		if p < 1 {
			t.Errorf("version advertises protocol %d: %s", p, c.body)
		}
	}
	// Created things can be read where Location says.
	c = callAPI(t, mux, "POST", "/api/v1/fleet/tokens", "good", `{"name":"terraform","role":"operator"}`)
	if c.code != 201 || callAPI(t, mux, "GET", c.header.Get("Location"), "good", "").code != 200 {
		t.Errorf("token's Location %q: %d", c.header.Get("Location"), c.code)
	}
	c = callAPI(t, mux, "POST", "/api/v1/fleet/members/ap/join-tokens", "op", "")
	if c.code != 201 || c.header.Get("Location") == "" || callAPI(t, mux, "GET", c.header.Get("Location"), "op", "").code != 200 {
		t.Errorf("join token's Location %q: %d %s", c.header.Get("Location"), c.code, c.body)
	}
	callAPI(t, mux, "PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "good", `{"role":"operator"}`)
	if c := callAPI(t, mux, "GET", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42", "op", ""); c.code != 200 || !strings.Contains(c.body, `"operator"`) {
		t.Errorf("one binding: %d %s", c.code, c.body)
	}
	// The bootstrap token is not revoked by the API: it says so.
	if c := callAPI(t, mux, "DELETE", "/api/v1/fleet/tokens/bootstrap", "good", ""); c.code != 409 {
		t.Errorf("revoking bootstrap: %d %s", c.code, c.body)
	}
	// JSON for unknown paths and methods.
	if c := callAPI(t, mux, "GET", "/api/v1/fleet/nothing", "good", ""); c.code != 404 || c.header.Get("Content-Type") != "application/json" {
		t.Errorf("unknown path: %d %s %s", c.code, c.header.Get("Content-Type"), c.body)
	}
	if c := callAPI(t, mux, "PATCH", "/api/v1/fleet/members/eu", "good", ""); c.code != 405 || c.header.Get("Content-Type") != "application/json" ||
		c.header.Get("Allow") != "GET, DELETE" {
		t.Errorf("unknown method: %d %v %s", c.code, c.header, c.body)
	}
	// Strict request bodies and parameters.
	for _, b := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/v1/fleet/members", addBody("eu", pubKey(t)) + "=garbage{{{", 400},
		{"POST", "/api/v1/fleet/members", `{"name":"eu","public_key":"` + strings.Repeat("x", 100<<10) + `"}`, 413},
		{"GET", "/api/v1/fleet/audit?limit=5000", "", 400},
		{"GET", "/api/v1/fleet/audit?before=x", "", 400},
		{"DELETE", "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/0", "", 400},
	} {
		if c := callAPI(t, mux, b.method, b.path, "good", b.body); c.code != b.want {
			t.Errorf("%s %s: %d, want %d: %.200s", b.method, b.path, c.code, b.want, c.body)
		}
	}
	// A join token's body is optional, also when sent chunked.
	req := httptest.NewRequest("POST", "http://localhost:8080/api/v1/fleet/members/ch/join-tokens", strings.NewReader(""))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer op")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Errorf("join token, chunked empty body: %d %s", rec.Code, rec.Body)
	}
	// Values of a member without settings say so.
	callAPI(t, mux, "POST", "/api/v1/fleet/members", "good", addBody("bare", pubKey(t)))
	if c := callAPI(t, mux, "GET", "/api/v1/fleet/members/bare/values", "good", ""); c.code != 404 || !strings.Contains(c.body, "no settings") {
		t.Errorf("values of a member without settings: %d %s", c.code, c.body)
	}
}

// A delivery marks delivered only what it carried, and only while the
// hub still keeps that value; a secret that does not open is left out,
// the others are delivered.
func TestDeliveredOutgoing(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	sealer, _ := NewSealer([]byte(testSecretsKey))
	vals := map[string]string{"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir", "runner_token": "glrt-not-a-real-token", "gitlab_token": "glpat-read"}
	if _, err := r.SaveSettings(ctx, sealer, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	o, err := r.Outgoing(ctx, sealer, "eu")
	if err != nil || o.Secrets[SecretRunner]["runner-token"] != "glrt-not-a-real-token" {
		t.Fatalf("outgoing: %v %v", o.Secrets, err)
	}
	// Saved again while that delivery is on its way.
	vals["runner_token"] = "glrt-another-real-not"
	if _, err := r.SaveSettings(ctx, sealer, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	if err := r.DeliveredOutgoing(ctx, "eu", o, []string{SecretRunner, SecretGitLab}); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.Undelivered(ctx, "eu"); !u {
		t.Error("a secret saved during a delivery counts as delivered")
	}
	s, _ := r.Settings(ctx, "eu")
	for _, k := range s.Secrets {
		if k.Param == "gitlab_token" && k.DeliveredAt.IsZero() {
			t.Error("the secret the delivery carried is not marked")
		}
	}
	// Only Secrets the member wrote count.
	o, _ = r.Outgoing(ctx, sealer, "eu")
	if err := r.DeliveredOutgoing(ctx, "eu", o, nil); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.Undelivered(ctx, "eu"); !u {
		t.Error("a Secret the member did not write counts as delivered")
	}
	// A key changed: what does not open is named and left out.
	other, _ := NewSealer([]byte("another-key-another-key-another-key"))
	vals["runner_token"] = "glrt-sealed-with-new-key"
	delete(vals, "gitlab_token")
	if _, err := r.SaveSettings(ctx, other, "x", "eu", vals); err != nil {
		t.Fatal(err)
	}
	o, err = r.Outgoing(ctx, other, "eu")
	if err != nil || o.Secrets[SecretRunner]["runner-token"] != "glrt-sealed-with-new-key" || strings.Join(o.Stale, " ") != "gitlab_token" {
		t.Errorf("after a key change: %v stale %v %v", o.Secrets, o.Stale, err)
	}
}

// PUT viewer takes the binding away and answers 204: no made-up binding
// (empty origin, zero added_at) for something never stored.
func TestGrantViewerRemovesTheBinding(t *testing.T) {
	r, _, mux := testAPI(t, nil)
	const path = "/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/42"
	if c := callAPI(t, mux, "PUT", path, "good", `{"role":"operator"}`); c.code != http.StatusOK {
		t.Fatalf("operator: %d %s", c.code, c.body)
	}
	c := callAPI(t, mux, "PUT", path, "good", `{"role":"viewer"}`)
	if c.code != http.StatusNoContent || c.body != "" {
		t.Fatalf("viewer: %d %q, want 204 and no body", c.code, c.body)
	}
	if c := callAPI(t, mux, "GET", path, "good", ""); c.code != http.StatusNotFound {
		t.Fatalf("after viewer: %d %s, want 404 (no binding)", c.code, c.body)
	}
	if c := callAPI(t, mux, "PUT", path, "good", `{"role":"viewer"}`); c.code != http.StatusNoContent {
		t.Fatalf("viewer again: %d %s", c.code, c.body)
	}
	es, err := r.Audit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	revokes := 0
	for _, e := range es {
		if e.Action == "role.revoke" && e.Target == "gitlab:gitlab.example.com/42" {
			revokes++
		}
	}
	if revokes != 1 {
		t.Fatalf("%d role.revoke records, want 1: %+v", revokes, es)
	}
}

// On a hub with sign-in, a grant to a host nobody signs in from is a
// binding that matches nobody: refused. Taking one away still works.
func TestGrantNeedsASignInHost(t *testing.T) {
	r, a, mux := testAPI(t, nil)
	a.SignInHosts = []string{"gitlab.example.com", "git.example.org:8443"}
	for _, c := range []struct {
		path string
		want int
	}{
		{"/api/v1/fleet/role-bindings/gitlab/gitlab.other.com/5", http.StatusBadRequest},
		{"/api/v1/fleet/role-bindings/gitlab/git.example.org/5", http.StatusBadRequest},
		{"/api/v1/fleet/role-bindings/gitlab/git.example.org:8443/5", http.StatusOK},
		{"/api/v1/fleet/role-bindings/gitlab/gitlab.example.com/5", http.StatusOK},
	} {
		got := callAPI(t, mux, "PUT", c.path, "good", `{"role":"operator"}`)
		if got.code != c.want {
			t.Errorf("PUT %s: %d %s, want %d", c.path, got.code, got.body, c.want)
		}
		if c.want == http.StatusBadRequest && !strings.Contains(got.body, "none of the GitLabs people sign in with") {
			t.Errorf("PUT %s: %s", c.path, got.body)
		}
	}
	if _, err := r.binding(context.Background(), "gitlab:gitlab.other.com/5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused grant was stored: %v", err)
	}
	if c := callAPI(t, mux, "DELETE", "/api/v1/fleet/role-bindings/gitlab/gitlab.other.com/5", "good", ""); c.code != http.StatusNoContent {
		t.Fatalf("DELETE of an unknown host: %d %s", c.code, c.body)
	}
	a.SignInHosts = nil
	if c := callAPI(t, mux, "PUT", "/api/v1/fleet/role-bindings/gitlab/gitlab.other.com/5", "good", `{"role":"operator"}`); c.code != http.StatusOK {
		t.Fatalf("no sign-in hosts: %d %s, want any host", c.code, c.body)
	}
}

// The version endpoint lists only the protocols the hub's link accepts.
func TestVersionListsAcceptedProtocols(t *testing.T) {
	_, _, mux := testAPI(t, nil)
	c := callAPI(t, mux, "GET", "/api/v1/fleet/version", "good", "")
	var v struct {
		Protocols []int `json:"protocols"`
	}
	if err := json.Unmarshal([]byte(c.body), &v); err != nil {
		t.Fatal(err)
	}
	want := Protocols()
	if len(v.Protocols) != len(want) {
		t.Fatalf("protocols %v, want %v", v.Protocols, want)
	}
	for i := range want {
		if v.Protocols[i] != want[i] || v.Protocols[i] < 1 {
			t.Fatalf("protocols %v, want %v", v.Protocols, want)
		}
	}
}
