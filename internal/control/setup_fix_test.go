package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A controller of another release knows other Config fields: the client
// sends back the setup digest the controller stated, not one of its own
// reading, so the calls are not refused for the difference.
func TestSetupAsTheControllerStatesIt(t *testing.T) {
	const stated = "0123456789abcdef"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Header().Set(SetupHeader, stated)
			w.Write([]byte(`{"Version":"v-next","Release":"box","AFieldOfANewerRelease":true}`))
			return
		}
		if got := r.Header.Get(SetupHeader); got != stated {
			http.Error(w, "setup "+got, http.StatusConflict)
			return
		}
		w.Write([]byte(`{"r":[[]]}`))
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects(ctx); err != nil {
		t.Fatalf("a call to a controller of another release: %v", err)
	}
}

// The controller states its digest with its Config.
func TestConfigStatesTheSetup(t *testing.T) {
	srv := &Server{Local: &Local{Store: newStore(t)}, Config: Config{Release: "box", GitLab: true}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	res, err := http.Get(ts.URL + "/internal/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var cfg Config
	if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get(SetupHeader); got == "" || got != srv.Config.Setup() {
		t.Fatalf("stated setup %q, want %q", got, srv.Config.Setup())
	}
}

// A method the controller does not have is named as a version difference.
func TestUnknownMethodNamesAnotherVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		http.Error(w, "no such method", http.StatusNotFound)
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.BuildHints(ctx, []int64{1})
	if err == nil || !strings.Contains(err.Error(), "BuildHints") || !strings.Contains(err.Error(), "another version") {
		t.Fatalf("an unknown method: %v", err)
	}
}
