package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// saveFailed stores a failed build recorded from a BuildKit v0.34.1
// daemon (the history package's testdata/<dir>) and returns its id.
func saveFailed(t *testing.T, st *store.Store, dir string) (int64, *history.Build) {
	t.Helper()
	b := fixtureBuild(t, st, filepath.Join("../history/testdata", dir))
	id, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	return id, b
}

// dfLine is line n of a page's Dockerfile view (the first one on the page).
func dfLine(body string, n int) string {
	for _, part := range strings.Split(body, `<div class="dl`)[1:] {
		part, _, _ = strings.Cut(part, "</div>")
		if strings.Contains(part, fmt.Sprintf(`<span class="dn">%d</span>`, n)) {
			return `<div class="dl` + part
		}
	}
	return ""
}

// A build of target "test" that failed in the stage "deps" before it
// reached "test": the test stage is not dimmed (a failed build cannot
// tell unreached stages from unused ones), the failed step's line is
// marked failed.
func TestFailedBuildDoesNotDim(t *testing.T) {
	ts, st, _ := console(t)
	id, b := saveFailed(t, st, "failed-target")
	p := get(t, ts, fmt.Sprintf("/builds/%d", id))
	noBlankName(t, p)
	if !strings.Contains(p.body, `data-tab="dockerfile"`) {
		t.Fatal("no Dockerfile tab")
	}
	if strings.Contains(p.body, "no Dockerfile") {
		t.Error("a build with its Dockerfile says it has none")
	}
	if strings.Contains(p.body, " dim\"") || strings.Contains(p.body, "dimmed:") || strings.Contains(p.body, "not part of") {
		t.Error("a failed build dims lines")
	}
	if l := dfLine(p.body, 3); !strings.Contains(l, "st-failed") || !strings.Contains(l, "RUN") {
		t.Errorf("line 3: %s", l)
	}
	for _, n := range []int{5, 6} {
		if l := dfLine(p.body, n); l == "" || strings.Contains(l, "dim") || strings.Contains(l, "st-") {
			t.Errorf("line %d: %s", n, l)
		}
	}
	// The same lines, before the template.
	_, text := b.Dockerfile()
	for _, l := range dockerfile(text, steps(b, history.Reason{}, func(string) string { return "" }, nil), b) {
		if l.Dim || l.Unparsed || (l.State == "failed") != (l.N == 3) {
			t.Errorf("line %d: %+v", l.N, l)
		}
	}
}

// A Dockerfile BuildKit could not parse: no step ran; the line its error
// names is marked, and the page says which.
func TestUnparsedDockerfile(t *testing.T) {
	ts, st, _ := console(t)
	id, _ := saveFailed(t, st, "failed-parse")
	p := get(t, ts, fmt.Sprintf("/builds/%d", id))
	noBlankName(t, p)
	if !strings.Contains(p.body, "No step ran: BuildKit could not parse line 3 of the Dockerfile.") {
		t.Error("the page does not say which line")
	}
	if strings.Count(p.body, "BuildKit could not parse this line") != 1 {
		t.Errorf("%d lines marked unparsed", strings.Count(p.body, "BuildKit could not parse this line"))
	}
	l := dfLine(p.body, 3)
	if !strings.Contains(l, `class="dl st-failed"`) || !strings.Contains(l, "FOOBAR") || strings.Contains(l, "data-step") {
		t.Errorf("line 3: %s", l)
	}
	for _, n := range []int{1, 2, 4} {
		if l := dfLine(p.body, n); l == "" || strings.Contains(l, "st-") {
			t.Errorf("line %d: %s", n, l)
		}
	}
}

// A canceled build has no Dockerfile on record, and the page says so,
// as it does for a failed build whose error names none.
func TestFailedBuildWithoutDockerfile(t *testing.T) {
	ts, st, _ := console(t)
	cid, _ := saveFailed(t, st, "failed-canceled")
	c := get(t, ts, fmt.Sprintf("/builds/%d", cid))
	noBlankName(t, c)
	if !strings.Contains(c.body, "BuildKit records no Dockerfile for a canceled build.") || strings.Contains(c.body, "BuildKit recorded no Dockerfile") ||
		strings.Contains(c.body, `data-tab="dockerfile"`) {
		t.Error("canceled build: no note, or the generic one")
	}

	// The failed RUN without its error blob: failed, and no Dockerfile.
	d := filepath.Join("../history/testdata", "failed-run")
	read := func(n string) []byte { b, _ := os.ReadFile(filepath.Join(d, n)); return b }
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.NewBuild(rec, progress, nil, nil, nil, history.Config{SecretKey: st.SecretKey()})
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = "daemon-a"
	fid, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	f := get(t, ts, fmt.Sprintf("/builds/%d", fid))
	noBlankName(t, f)
	if !strings.Contains(f.body, "BuildKit recorded no Dockerfile for this build.") || strings.Contains(f.body, "canceled build") {
		t.Error("failed build without a Dockerfile: no note, or the canceled one")
	}
	// A build with its Dockerfile says nothing of the kind.
	pid, _ := saveFailed(t, st, "failed-parse")
	if p := get(t, ts, fmt.Sprintf("/builds/%d", pid)); strings.Contains(p.body, "no Dockerfile") {
		t.Error("a build with its Dockerfile says it has none")
	}

	// Compared, each build without a Dockerfile is named with the reason.
	cmp := get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", cid, pid))
	noBlankName(t, cmp)
	if strings.Count(cmp.body, "no Dockerfile") != 1 || !strings.Contains(cmp.body, ": BuildKit records no Dockerfile for a canceled build.</p>") {
		t.Errorf("compare canceled/parse: %s", regexp.MustCompile(`(?s)id="cdT".*?</section>`).FindString(cmp.body))
	}
	cmp = get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", cid, fid))
	if !strings.Contains(cmp.body, ": BuildKit records no Dockerfile for a canceled build.</p>") ||
		!strings.Contains(cmp.body, ": BuildKit recorded no Dockerfile for this build.</p>") {
		t.Errorf("compare canceled/failed: %s", regexp.MustCompile(`(?s)id="cdT".*?</section>`).FindString(cmp.body))
	}
}

// A token written into a Dockerfile is masked on the build page's
// Dockerfile tab and in the step drawer's source lines, as it is in the
// step's name; a private key keeps its lines, so the lines after it keep
// their numbers.
func TestDockerfileSecretsMasked(t *testing.T) {
	ts, st, _ := console(t)
	src, _ := filepath.Glob("../history/testdata/records/39usrwksrt7qy8g8n71g38pf2")
	dir := t.TempDir()
	lastLine := 0
	for _, n := range []string{"record.json", "logs.bin", "provenance.json", "trace.json"} {
		b, err := os.ReadFile(filepath.Join(src[0], n))
		if err != nil {
			t.Fatal(err)
		}
		if n == "provenance.json" {
			var prov map[string]any
			if err := json.Unmarshal(b, &prov); err != nil {
				t.Fatal(err)
			}
			info := prov["runDetails"].(map[string]any)["metadata"].(map[string]any)["buildkit_metadata"].(map[string]any)["source"].(map[string]any)["infos"].([]any)[0].(map[string]any)
			text, _ := base64.StdEncoding.DecodeString(info["data"].(string))
			secret := strings.Replace(string(text), "RUN sleep 1 && echo more > /more",
				`RUN sleep 1 && printf 'glpat-ABCDEFGHIJKLMNOPQRST\nghp_`+strings.Repeat("a1", 18)+`\nAKIAABCDEFGHIJKLMNOP\n' && echo more > /more`, 1)
			secret += "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----\n# last\n"
			if !strings.Contains(secret, "glpat-") {
				t.Fatal("the fixture's Dockerfile changed")
			}
			lastLine = strings.Count(secret[:strings.Index(secret, "# last")], "\n") + 1
			info["data"] = base64.StdEncoding.EncodeToString([]byte(secret))
			if b, err = json.Marshal(prov); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := fixtureBuild(t, st, dir)
	id, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	step := -1
	for i, s := range b.Steps {
		if strings.Contains(s.Name, "echo more") {
			step = i
		}
	}
	if step < 0 {
		t.Fatal("no step for the line")
	}
	leaks := regexp.MustCompile(`glpat-|ghp_|AKIA|b3Blbn`)
	p := get(t, ts, fmt.Sprintf("/builds/%d", id))
	if !strings.Contains(p.body, `data-tab="dockerfile"`) {
		t.Fatal("no Dockerfile tab")
	}
	if m := leaks.FindString(p.body); m != "" {
		t.Errorf("the build page shows %q", m)
	}
	if l := dfLine(p.body, lastLine); !strings.Contains(l, "# last") {
		t.Errorf("line %d after the key: %s", lastLine, l)
	}
	d := get(t, ts, fmt.Sprintf("/builds/%d/steps/%d", id, step))
	if d.status != 200 || !strings.Contains(d.body, "[masked]") {
		t.Errorf("step %d: %d, nothing masked", step, d.status)
	}
	if m := leaks.FindString(d.body); m != "" {
		t.Errorf("the step shows %q", m)
	}
}
