package web

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The worker's repository carries markup: it reaches the pages as text,
// in titles too.
const (
	apiRepo    = "registry.example.com:5000/g/api"
	workerRepo = `registry.example.com:5000/g/worker"onmouseover=alert(1)`
)

// withoutTarget saves, into the console's store, builds of the project's
// two Dockerfiles without a target (the CI component's default), each
// pushing its commit's tag and latest in one attribute as the component
// does, plus one that pushed nothing. It returns their ids by name.
func withoutTarget(t *testing.T, st *store.Store) map[string]int64 {
	t.Helper()
	var base *history.Build
	dirs, _ := filepath.Glob("../history/testdata/records/*")
	for _, d := range dirs {
		if b := fixtureBuild(t, st, d); b.Target == "api" && b.Attribution.State == history.Attributed && b.Error == "" {
			built := slices.ContainsFunc(b.Steps, func(s history.Step) bool { return s.State == history.Built && !isFrom(s) })
			if _, text := b.Dockerfile(); text != "" && built && (base == nil || b.Started.After(base.Started)) {
				base = b
			}
		}
	}
	if base == nil {
		t.Fatal("the fixtures lost the case this test needs: a recorded api build with its Dockerfile that ran steps")
	}
	ids := map[string]int64{}
	for i, sp := range []struct{ name, repo string }{
		{"api1", apiRepo}, {"worker1", workerRepo}, {"api2", apiRepo}, {"local", ""}, {"api3", apiRepo},
	} {
		b := *base
		b.Ref, b.Target = "notarget-"+sp.name, ""
		b.Attribution.Pipeline, b.Attribution.Job = uint64(9_300_001+i), uint64(9_400_001+i)
		b.Started = base.Started.Add(time.Duration(i+1) * time.Minute)
		b.Completed = b.Started.Add(3 * time.Second)
		b.Images = nil
		if sp.repo != "" {
			b.Images = []string{fmt.Sprintf("%s:c%d,%s:latest", sp.repo, i, sp.repo)}
		}
		err := st.SaveJob(ctx, store.Job{Instance: b.Attribution.Instance, JobID: b.Attribution.Job, PipelineID: b.Attribution.Pipeline,
			ProjectID: 42, ProjectPath: "example/" + evil, Name: "build-" + sp.name, Stage: "build", CommitTitle: "c " + sp.name})
		if err != nil {
			t.Fatal(err)
		}
		prev, _, err := st.Previous(ctx, &b)
		if err != nil {
			t.Fatal(err)
		}
		var p *history.Build
		if prev != nil {
			p = prev.Build
		}
		r, _ := history.FirstRebuild(p, &b)
		id, err := st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1, Reason: r})
		if err != nil {
			t.Fatal(err)
		}
		ids[sp.name] = id
	}
	return ids
}

// fixtureBuild reads a recorded build of the history package's testdata
// (dir), keyed with the store's secret key, on daemon-a.
func fixtureBuild(t *testing.T, st *store.Store, dir string) *history.Build {
	t.Helper()
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	cfg := history.Config{Instances: map[string]bool{history.InstancePrefix("https://gitlab.example.com/"): true}, SecretKey: st.SecretKey()}
	b, err := history.NewBuild(rec, progress, read("provenance.json"), read("trace.json"), read("error.bin"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = "daemon-a"
	return b
}

// noBlankName fails on the words of the old naming and on a name that
// rendered empty.
func noBlankName(t *testing.T, p page) {
	t.Helper()
	p.safe(t)
	if p.status != 200 {
		t.Errorf("%s: %d", p.url, p.status)
	}
	for _, bad := range []string{"default target", "<code></code>", "not part of target <", "not part of target  "} {
		if strings.Contains(p.body, bad) {
			t.Errorf("%s says %q", p.url, bad)
		}
	}
}

// escaped is s as html/template writes it in text and attributes.
func escaped(s string) string {
	return strings.ReplaceAll(template.HTMLEscapeString(s), "+", "&#43;")
}

var buildLink = regexp.MustCompile(`href="/builds/(\d+)"`)

func TestImagesWithoutTarget(t *testing.T) {
	ts, st, _ := console(t)
	ids := withoutTarget(t, st)
	esc := escaped(workerRepo)
	more := escaped(" +1")

	// The images list: one row for the builds without a target, named by
	// the latest repository and counting the other, every one in its title.
	list := get(t, ts, "/images?window=7d")
	noBlankName(t, list)
	row := regexp.MustCompile(`<a class="link" href="/images/-"[^>]*>[^<]*</a>`).FindString(list.body)
	if want := fmt.Sprintf(`<a class="link" href="/images/-" title="%s, %s">%s%s</a>`, apiRepo, esc, apiRepo, more); row != want {
		t.Errorf("no-target row:\n got %s\nwant %s", row, want)
	}
	if strings.Contains(list.body, noTarget) {
		t.Error("the images list says last stage although builds pushed images")
	}

	// The image page lists only builds without a target.
	img := get(t, ts, "/images/-?window=7d")
	noBlankName(t, img)
	if !strings.Contains(img.body, fmt.Sprintf(`<h1 class="t" tabindex="-1" title="%s, %s">%s%s</h1>`, apiRepo, esc, apiRepo, more)) {
		t.Errorf("image page heading: %s", regexp.MustCompile(`<h1[^>]*>[^<]*</h1>`).FindString(img.body))
	}
	if !strings.Contains(img.body, "of 5 builds in the last 7 days") || strings.Count(img.body, `class="dot st-`) != 5 {
		t.Errorf("image page: %d dots", strings.Count(img.body, `class="dot st-`))
	}

	// The builds list filtered to "no target": the select keeps "-".
	bl := get(t, ts, "/builds?image=-")
	noBlankName(t, bl)
	if !strings.Contains(bl.body, fmt.Sprintf(`<option value="-" selected title="%s, %s">%s%s</option>`, apiRepo, esc, apiRepo, more)) {
		t.Errorf("image select: %s", regexp.MustCompile(`(?s)<select name="image".*?</select>`).FindString(bl.body))
	}
	var listed []int64
	for _, m := range buildLink.FindAllStringSubmatch(bl.body, -1) {
		var id int64
		fmt.Sscan(m[1], &id)
		listed = append(listed, id)
	}
	for name, id := range ids {
		if !slices.Contains(listed, id) {
			t.Errorf("/builds?image=- lacks %s", name)
		}
	}
	if len(listed) != len(ids) {
		t.Errorf("/builds?image=- lists %d builds, want the %d without a target", len(listed), len(ids))
	}

	// Each build is named by what it pushed; Pushed lists the names one
	// by one.
	b := get(t, ts, fmt.Sprintf("/builds/%d", ids["api2"]))
	noBlankName(t, b)
	if !strings.Contains(b.body, fmt.Sprintf("<dd>%s:c2, %s:latest</dd>", apiRepo, apiRepo)) {
		t.Errorf("Pushed: %s", regexp.MustCompile(`<dt>Pushed</dt><dd>[^<]*</dd>`).FindString(b.body))
	}
	// Stages the Dockerfile's last stage does not use are "not part of
	// this build"; the verdict speaks of "this image".
	if !strings.Contains(b.body, `<span class="mtag">not part of this build</span>`) || !strings.Contains(b.body, "dimmed: not part of this build") {
		t.Error("no-target build: dimmed stages not named")
	}
	for _, id := range ids {
		noBlankName(t, get(t, ts, fmt.Sprintf("/builds/%d", id)))
		noBlankName(t, get(t, ts, fmt.Sprintf("/builds/%d/compare", id)))
	}
	// The overview's reasons name both images without a target.
	ov := get(t, ts, "/?window=7d")
	noBlankName(t, ov)
	found := false
	for _, r := range regexp.MustCompile(`<span class="ri muted">([^<]*) · last at`).FindAllStringSubmatch(ov.body, -1) {
		found = found || strings.Contains(r[1], apiRepo) && strings.Contains(r[1], esc)
	}
	if !found {
		t.Error("no reason names both images without a target")
	}
	noBlankName(t, get(t, ts, "/builds"))
}

// Comparing a build without a target offers, and accepts, only builds of
// its own repository.
func TestCompareWithoutTarget(t *testing.T) {
	ts, st, _ := console(t)
	ids := withoutTarget(t, st)
	c := get(t, ts, fmt.Sprintf("/builds/%d/compare", ids["api3"]))
	noBlankName(t, c)
	picks := regexp.MustCompile(`/builds/\d+/compare\?with=(\d+)`).FindAllStringSubmatch(c.body, -1)
	got := map[string]bool{}
	for _, m := range picks {
		for name, id := range ids {
			if m[1] == fmt.Sprint(id) {
				got[name] = true
			}
		}
		if !slices.ContainsFunc([]string{"api1", "api2"}, func(n string) bool { return m[1] == fmt.Sprint(ids[n]) }) {
			t.Errorf("pick %s is not a build of %s", m[1], apiRepo)
		}
	}
	if !got["api1"] || !got["api2"] {
		t.Errorf("picks %v, want api1 and api2", got)
	}
	// The previous build of api3 is api2 (not local, not the worker).
	if !strings.Contains(c.body, fmt.Sprintf("compare?with=%d", ids["api2"])) {
		t.Error("api2 not offered")
	}
	refused := get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", ids["api3"], ids["worker1"]))
	if refused.status != 400 || !strings.Contains(refused.body, "only two builds of one image are compared") {
		t.Errorf("compare with the worker: %d", refused.status)
	}
	ok := get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", ids["api3"], ids["api1"]))
	noBlankName(t, ok)

	// Picking from the list: the first pick's repository only.
	pick := get(t, ts, fmt.Sprintf("/builds?pick=1&a=%d", ids["api3"]))
	noBlankName(t, pick)
	for _, m := range regexp.MustCompile(`/builds/\d+/compare\?with=(\d+)`).FindAllStringSubmatch(pick.body, -1) {
		if m[1] != fmt.Sprint(ids["api1"]) && m[1] != fmt.Sprint(ids["api2"]) {
			t.Errorf("the pick list offers %s", m[1])
		}
	}
	if !strings.Contains(pick.body, fmt.Sprintf("compare?with=%d", ids["api1"])) {
		t.Error("the pick list lacks api1")
	}
}

// imageName counts the repositories behind it; only builds that pushed
// nothing are "the last stage".
func TestImageName(t *testing.T) {
	for _, c := range []struct {
		target      string
		repos       []string
		name, title string
	}{
		{"app", []string{"r/a"}, "app", ""},
		{"", nil, noTarget, ""},
		{"", []string{"r/a"}, "r/a", ""},
		{"", []string{"r/a", "r/b", "r/c"}, "r/a +2", "r/a, r/b, r/c"},
	} {
		if n, ti := imageName(c.target, c.repos), imageTitle(c.target, c.repos); n != c.name || ti != c.title {
			t.Errorf("%q %q: %q %q, want %q %q", c.target, c.repos, n, ti, c.name, c.title)
		}
	}
	if got := buildName("", []string{"r/a:1,r/a:latest", "r/b:2"}); got != "r/a +1" {
		t.Errorf("buildName: %q", got)
	}
}

// A verdict speaks of "this target" only when the build has one.
func TestVerdictNamesTheImage(t *testing.T) {
	step := history.Step{Key: "[2/3]", Name: "RUN make", Line: 4}
	for _, target := range []string{"api", ""} {
		want, wrong := "this target", "this image"
		if target == "" {
			want, wrong = wrong, want
		}
		b := &history.Build{Target: target}
		for _, r := range []history.Reason{
			{Kind: history.OtherNode, Step: step, Old: "d-a", New: "d-b"},
			{Kind: history.CacheElsewhere, Step: step, Old: "d-a", New: "d-b", Paths: []string{"src"}},
		} {
			for _, by := range []string{"", "build-other"} {
				v := verdictFor(b, r, by, 1, 0, nil)
				text := string(v.Title) + " " + string(v.Detail)
				if !strings.Contains(text, "where "+want+" was built last time") || strings.Contains(text, wrong) {
					t.Errorf("target %q, %s, by %q: %s", target, r.Kind, by, text)
				}
			}
		}
		v := verdictFor(b, history.Reason{}, "", 2, 0, nil)
		if !strings.Contains(string(v.Detail), "No earlier build of "+want+" is on record") {
			t.Errorf("target %q, not compared: %s", target, v.Detail)
		}
	}
}

// A busy Dockerfile without a target does not leave the pick list of a
// quiet one empty: the list reads on until it holds builds of the first
// pick's repository (0.3.2 review: 60 newer worker builds, an empty page
// beside an "Older" link).
func TestPickListReadsOn(t *testing.T) {
	ts, st, _ := console(t)
	ids := withoutTarget(t, st)
	worker, err := st.BuildPage(ctx, ids["worker1"])
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		b := *worker.Build
		b.Ref = fmt.Sprintf("busy-worker-%d", i)
		b.Attribution.Pipeline, b.Attribution.Job = uint64(9_500_001+i), uint64(9_600_001+i)
		b.Started = b.Started.Add(time.Duration(10+i) * time.Hour)
		b.Completed = b.Started.Add(3 * time.Second)
		if err := st.SaveJob(ctx, store.Job{Instance: b.Attribution.Instance, JobID: b.Attribution.Job, PipelineID: b.Attribution.Pipeline,
			ProjectID: 42, ProjectPath: "example/" + evil, Name: "build-worker", Stage: "build"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	pick := get(t, ts, fmt.Sprintf("/builds?pick=1&a=%d", ids["api3"]))
	if !strings.Contains(pick.body, fmt.Sprintf("compare?with=%d", ids["api1"])) || !strings.Contains(pick.body, fmt.Sprintf("compare?with=%d", ids["api2"])) {
		t.Error("the pick list does not reach the api builds behind 60 newer worker builds")
	}
	if strings.Contains(pick.body, "busy-worker") || strings.Contains(pick.body, "No build matches") {
		t.Error("the pick list shows the worker's builds or says nothing matches")
	}
}
