package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// A pipeline read from GitLab's API carries its commit, so a pipeline whose
// job pods were never seen still has one to compare against.
func TestPipelineCarriesItsCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":11,"name":"build","stage":"build","status":"success",
 "pipeline":{"id":7,"project_id":42,"sha":"0123456789abcdef0123456789abcdef01234567","status":"success","web_url":"https://gl/x/-/pipelines/7"}}]`)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	p, err := c.Pipeline(context.Background(), Token("t"), history.InstancePrefix(srv.URL), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("pipeline commit %q", p.SHA)
	}
}
