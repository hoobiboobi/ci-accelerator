package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Fuzz targets for the pages a fleet hub serves: a cluster's pages under
// its base (/m/<name>/), the redirects made from what a request carries,
// what the hub's own pages show to whom, and the time axes laid out from
// a cluster's instants. Everything a cluster answers comes from another
// process the hub does not control, so every string and instant here is
// the fuzzer's.

// hfBound is how long one input may take; the work takes well under 10 ms,
// so passing it is a hang.
const hfBound = 3 * time.Second

var hfLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// hfWithin runs work under the bound and reports what it found; work runs
// in its own goroutine so that a hang is reported, not waited out.
func hfWithin(t *testing.T, what string, work func() []string) {
	t.Helper()
	done := make(chan []string, 1)
	go func() { done <- work() }()
	select {
	case errs := <-done:
		if len(errs) > 0 {
			t.Fatalf("%s:\n%s", what, strings.Join(errs, "\n"))
		}
	case <-time.After(hfBound):
		t.Fatalf("%s: no answer within %s (a hang)", what, hfBound)
	}
}

// hfFleet is a hub's fleet whose members answer what the input says.
type hfFleet struct {
	current string
	members []FleetMember
	hits    []FleetHit
	silent  []string
}

func (f *hfFleet) Members() []FleetMember { return f.members }
func (f *hfFleet) Current() string        { return f.current }
func (f *hfFleet) Locate(context.Context, string, uint64, uint64) ([]FleetHit, []string) {
	return f.hits, f.silent
}

// Markers of the member details only Members (operators, owners) shows.
const (
	hfPrintMark  = "SHA256:FPMARK"
	hfRemoteMark = "REMOTEMARK"
	hfAddedMark  = "ADDEDBYMARK"
)

// hfMembers: eu and us connected, ap away, old removed.
func hfMembers() []FleetMember {
	m := func(name, state string) FleetMember {
		return FleetMember{Name: name, State: state, Version: "0.8.0", Since: testNow.Add(-time.Hour), Fingerprint: hfPrintMark + name,
			Remote: hfRemoteMark, Origin: "api", AddedBy: "token:" + hfAddedMark, AddedAt: testNow.Add(-2 * time.Hour)}
	}
	ap, old := m("ap", "offline"), m("old", "removed")
	ap.SeenAt, old.RemovedAt = testNow.Add(-14*time.Minute), testNow.Add(-48*time.Hour)
	return []FleetMember{m("eu", "connected"), m("us", "connected"), ap, old}
}

// hfBackend is a member's store as it answers the hub: one project (ID 7)
// whose every string is the fuzzer's, with one pipeline (50), one job (9)
// and one image, next to a plain project (8).
type hfBackend struct {
	Empty
	proj, safe store.Project
	pipe       store.Pipeline
	row        store.PipeRow
	jobs       []store.JobInfo
	build      store.BuildRow
	image      string
	// imageIn: the project that has the image (the other one does not).
	imageIn store.ProjectKey
}

func (b *hfBackend) Projects(context.Context) ([]store.Project, error) {
	return []store.Project{b.safe, b.proj}, nil
}
func (b *hfBackend) Branches(_ context.Context, p store.ProjectKey, _ time.Time) ([]string, error) {
	if p == b.proj.ProjectKey {
		return []string{b.row.Ref}, nil
	}
	return nil, nil
}
func (b *hfBackend) Targets(_ context.Context, p store.ProjectKey, _ time.Time) ([]string, error) {
	if p == b.imageIn {
		return []string{b.image}, nil
	}
	return nil, nil
}
func (b *hfBackend) PipelineList(_ context.Context, q store.PipeQuery) ([]store.PipeRow, error) {
	if q.Project == b.proj.ProjectKey {
		return []store.PipeRow{b.row}, nil
	}
	return nil, nil
}
func (b *hfBackend) JobsOf(_ context.Context, instance string, _ []uint64) (map[uint64][]store.JobInfo, error) {
	if instance == b.proj.Instance {
		return map[uint64][]store.JobInfo{b.row.ID: b.jobs}, nil
	}
	return nil, nil
}
func (b *hfBackend) PipelineInfo(_ context.Context, instance string, id uint64) (*store.Pipeline, error) {
	if instance == b.proj.Instance && id == b.pipe.ID {
		p := b.pipe
		return &p, nil
	}
	return nil, nil
}
func (b *hfBackend) PipelineProject(_ context.Context, instance string, id uint64) (uint64, error) {
	if instance == b.proj.Instance && id == b.pipe.ID {
		return b.proj.ID, nil
	}
	return 0, nil
}
func (b *hfBackend) PipelineJobs(_ context.Context, instance string, id uint64) ([]store.JobInfo, error) {
	if instance == b.proj.Instance && id == b.pipe.ID {
		return b.jobs, nil
	}
	return nil, nil
}
func (b *hfBackend) Job(_ context.Context, instance string, id uint64) (*store.JobInfo, error) {
	for _, j := range b.jobs {
		if instance == j.Instance && id == j.ID {
			return &j, nil
		}
	}
	return nil, nil
}

// Builds answers the search index's query only (the pages with a build
// row read its whole build, which this store does not have).
func (b *hfBackend) Builds(_ context.Context, q store.BuildQuery) ([]store.BuildRow, error) {
	if q.Project == b.proj.ProjectKey && q.Limit == 500 && !q.Light {
		return []store.BuildRow{b.build}, nil
	}
	return nil, nil
}

// hfStrings are a member's strings for one input.
type hfStrings struct {
	Instance, Path, Branch, Title, Job, Stage, URL, Image string
}

// hfTwin replaces, byte for byte, what could start markup or end an
// attribute with characters that cannot: the twin renders the same
// elements unless the original injected one.
func hfTwin(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch c {
		case '<':
			b[i] = '('
		case '>':
			b[i] = ')'
		case '"', '\'', '`':
			b[i] = '_'
		case '&':
			b[i] = '+'
		}
	}
	return string(b)
}

func (s hfStrings) twin() hfStrings {
	return hfStrings{hfTwin(s.Instance), hfTwin(s.Path), hfTwin(s.Branch), hfTwin(s.Title), hfTwin(s.Job), hfTwin(s.Stage), hfTwin(s.URL), hfTwin(s.Image)}
}

var hfStatuses = []string{"success", "running", "failed", ""}

func hfBackendOf(s hfStrings, flags uint8) *hfBackend {
	created := testNow.Add(-10 * time.Minute)
	status := hfStatuses[flags&3]
	j := store.JobInfo{Instance: s.Instance, ID: 9, PipelineID: 50, ProjectID: 7, Name: s.Job, Stage: s.Stage, URL: s.URL,
		Started: created.Add(time.Minute), Status: status, Queued: created, Runner: s.Branch, StartedBy: s.Job}
	if flags&4 == 0 {
		j.Finished, j.Duration = j.Started.Add(41*time.Second), 41*time.Second
	}
	if flags&8 != 0 {
		j.Waited = 12 * time.Second
	}
	other := j
	other.ID, other.Name, other.Stage, other.Started, other.Finished = 10, s.Title, s.Path, time.Time{}, time.Time{}
	proj := store.Project{ProjectKey: store.ProjectKey{Instance: s.Instance, ID: 7}, Path: s.Path, Last: testNow}
	b := &hfBackend{proj: proj, safe: store.Project{ProjectKey: store.ProjectKey{Instance: "0a1b2c", ID: 8}, Path: "group/plain", Last: testNow},
		pipe: store.Pipeline{Instance: s.Instance, ID: 50, ProjectID: 7, ProjectPath: s.Path, Ref: s.Branch, SHA: s.Title, Created: created,
			CommitTitle: s.Title, CommitAuthor: s.Job + " <a@example.com>", Status: status, WebURL: s.URL},
		row: store.PipeRow{Instance: s.Instance, ID: 50, Ref: s.Branch, SHA: s.Title, Title: s.Title, Author: s.Job + " <a@example.com>",
			Created: created, Status: status, WebURL: s.URL},
		jobs:  []store.JobInfo{j, other},
		build: store.BuildRow{ID: 1, Instance: s.Instance, Pipeline: 50, Job: 9, Target: s.Image, Started: j.Started, Completed: j.Started.Add(time.Second), Title: s.Title, Ref: s.Branch, SHA: s.Title, JobName: s.Job},
		image: s.Image, imageIn: proj.ProjectKey}
	return b
}

// hfResult is one page as a member's pages answered it.
type hfResult struct {
	name, path string
	code       int
	loc, body  string
	html       bool
}

// hfMemberPages renders a member's pages at /m/eu for one store: the
// pipelines (home), a pipeline, a job, an image of the other project (it
// redirects there) and the search index.
func hfMemberPages(b *hfBackend) ([]hfResult, error) {
	srv, err := New(b, hfLog, Options{Base: "/m/eu", Fleet: &hfFleet{current: "eu", members: hfMembers()}, Version: "test"})
	if err != nil {
		return nil, err
	}
	srv.now = func() time.Time { return testNow }
	inst := b.proj.Instance
	type pg struct {
		name, path string
		vals       map[string]string
		h          http.HandlerFunc
		html       bool
	}
	img := imageKey(b.image)
	pages := []pg{
		{"pipelines", "/?project=" + url.QueryEscape(b.proj.String()), nil, srv.pipelines, true},
		{"pipeline", "/pipelines/" + url.PathEscape(inst) + "/50", map[string]string{"instance": inst, "id": "50"}, srv.pipeline, true},
		{"job", "/jobs/" + url.PathEscape(inst) + "/9", map[string]string{"instance": inst, "id": "9"}, srv.job, true},
		{"image", "/images/" + url.PathEscape(img), map[string]string{"target": img}, srv.image, true},
		{"search index", "/search-index?project=" + url.QueryEscape(b.proj.String()), nil, srv.index, false},
	}
	var out []hfResult
	for _, p := range pages {
		ctx, cancel := context.WithTimeout(context.Background(), hfBound)
		r := httptest.NewRequest("GET", p.path, nil).WithContext(ctx)
		for k, v := range p.vals {
			r.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		p.h(rec, r)
		cancel()
		out = append(out, hfResult{name: p.name, path: "/m/eu" + r.URL.EscapedPath(), code: rec.Code, loc: rec.Header().Get("Location"), body: rec.Body.String(), html: p.html})
	}
	return out, nil
}

// hfResolve is a path as a browser resolves its dot segments ("%2e"
// counts as a dot, a backslash as a slash).
func hfResolve(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	segs := strings.Split(p, "/")
	var out []string
	for i, s := range segs {
		if i == 0 && s == "" {
			continue
		}
		switch strings.ReplaceAll(strings.ToLower(s), "%2e", ".") {
		case ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, s)
	}
	return "/" + strings.Join(out, "/")
}

func hfUnder(p, base string) bool { return p == base || strings.HasPrefix(p, base+"/") }

// hfPathOf is an address without its query and fragment.
func hfPathOf(v string) string {
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		return v[:i]
	}
	return v
}

var hfScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// hfHubLink says whether p (resolved, no dot segments) is one of the hub's
// own pages that a member's page may link to.
func hfHubLink(p string, members []FleetMember) bool {
	switch {
	case p == "/", p == "/access", p == "/audit", p == "/clusters",
		strings.HasPrefix(p, "/static/"), strings.HasPrefix(p, "/auth/"), strings.HasPrefix(p, "/access/"),
		strings.HasPrefix(p, "/clusters/"):
		return true
	}
	for _, m := range members {
		if hfUnder(p, "/m/"+m.Name) {
			return true
		}
	}
	return false
}

// hfBadAddr says what is wrong with an address in a member's page served
// at page under base; "" when nothing is. Allowed: an anchor, an http(s)
// address, an address under the base that stays under it once resolved,
// and a link to the hub's own pages as written (no dot segments).
func hfBadAddr(v, page, base string, members []FleetMember) string {
	// As a browser reads it: tabs and newlines dropped, spaces trimmed.
	v = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, v)
	v = strings.TrimFunc(v, func(r rune) bool { return r <= ' ' })
	low := strings.ToLower(v)
	switch {
	case v == "", strings.HasPrefix(v, "#"):
		return ""
	case strings.HasPrefix(low, "https://"), strings.HasPrefix(low, "http://"):
		return ""
	case strings.HasPrefix(v, "//"), strings.HasPrefix(v, "/\\"), strings.HasPrefix(v, "\\"):
		return "protocol-relative"
	case !strings.HasPrefix(v, "/") && hfScheme.MatchString(v):
		return "a scheme other than http(s)"
	}
	p := hfPathOf(v)
	abs := p
	if !strings.HasPrefix(p, "/") {
		// Relative: from the page's own directory.
		abs = page[:strings.LastIndex(page, "/")+1] + p
	}
	res := hfResolve(abs)
	if hfUnder(abs, base) {
		for _, twice := range []string{base + "/m/", base + "/~", base + base + "/", base + "/static/"} {
			if strings.HasPrefix(abs, twice) {
				return "the base doubled or a hub link moved under it"
			}
		}
		if !hfUnder(res, base) {
			return "leaves the base: resolves to " + res
		}
		return ""
	}
	if res != strings.ReplaceAll(abs, "\\", "/") || strings.Contains(abs, "\\") {
		return "a hub link with dot segments: resolves to " + res
	}
	if !hfHubLink(res, members) {
		return "outside the base"
	}
	return ""
}

var (
	hfAddrAttrs = map[string]bool{"href": true, "src": true, "action": true, "data-url": true, "data-to": true, "formaction": true,
		"poster": true, "xlink:href": true, "data-href": true}
	hfBadTags = map[string]bool{"iframe": true, "object": true, "embed": true, "base": true, "frame": true, "frameset": true, "applet": true}
	// The console's own scripts: static files, by their versioned address.
	hfOwnScript = regexp.MustCompile(`^/static/[A-Za-z0-9._-]+\.js(\?v=[0-9a-f]+)?$`)
)

// hfCheckPage checks a rendered page and returns its elements (tag and
// attribute names, sorted) for the comparison with the twin.
func hfCheckPage(body, page, base string, members []FleetMember) (elems []string, errs []string) {
	z := html.NewTokenizer(strings.NewReader(body))
	inScript := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				errs = append(errs, "tokenizer: "+z.Err().Error())
			}
			sort.Strings(elems)
			return elems, errs
		case html.TextToken:
			if inScript && strings.TrimSpace(string(z.Text())) != "" {
				errs = append(errs, fmt.Sprintf("inline script code %q", short(string(z.Text()), 80)))
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "script" {
				inScript = false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			var names []string
			src := ""
			for _, a := range tok.Attr {
				k := strings.ToLower(a.Key)
				if a.Namespace != "" {
					k = a.Namespace + ":" + k
				}
				names = append(names, k)
				switch {
				case strings.HasPrefix(k, "on"):
					errs = append(errs, fmt.Sprintf("<%s %s=%q>: an event handler attribute", tok.Data, k, short(a.Val, 80)))
				case hfAddrAttrs[k]:
					if why := hfBadAddr(a.Val, page, base, members); why != "" {
						errs = append(errs, fmt.Sprintf("<%s %s=%q>: %s", tok.Data, k, short(a.Val, 120), why))
					}
				case k == "style":
					if l := strings.ToLower(a.Val); strings.Contains(l, "url(") || strings.Contains(l, "expression(") || strings.Contains(l, "javascript") {
						errs = append(errs, fmt.Sprintf("<%s style=%q>: loads or runs something", tok.Data, short(a.Val, 80)))
					}
				case k == "http-equiv":
					errs = append(errs, fmt.Sprintf("<%s http-equiv=%q>", tok.Data, a.Val))
				}
				if k == "src" {
					src = a.Val
				}
			}
			if hfBadTags[tok.Data] {
				errs = append(errs, "a <"+tok.Data+"> element")
			}
			if tok.Data == "script" {
				if !hfOwnScript.MatchString(src) {
					errs = append(errs, fmt.Sprintf("a script that is not the console's own: src=%q", src))
				}
				inScript = tt == html.StartTagToken
			}
			sort.Strings(names)
			elems = append(elems, tok.Data+"["+strings.Join(names, " ")+"]")
		}
	}
}

// hfBadLocation says what is wrong with a redirect's Location; "" when
// nothing is. A Location is a path of this origin: one leading slash, no
// scheme or host, no control character, and with a base, under it once
// resolved.
func hfBadLocation(loc, base string) string {
	if loc == "" {
		return "no Location"
	}
	for i := 0; i < len(loc); i++ {
		if loc[i] < 0x20 || loc[i] == 0x7f {
			return "a control character"
		}
	}
	if loc[0] != '/' {
		return "not a path from the root"
	}
	if len(loc) > 1 && (loc[1] == '/' || loc[1] == '\\') {
		return "protocol-relative"
	}
	// One that does not parse (a stray "%") is still a path from the
	// root to a browser.
	if u, err := url.Parse(loc); err == nil && (u.Scheme != "" || u.Host != "" || u.User != nil) {
		return "names a scheme or a host"
	}
	if base != "" {
		p := hfPathOf(loc)
		if !hfUnder(p, base) {
			return "not under " + base
		}
		if res := hfResolve(p); !hfUnder(res, base) {
			return "resolves to " + res + ", outside " + base
		}
	}
	return ""
}

// hfElemDiff names the elements one render has more of than the other.
func hfElemDiff(a, b []string) string {
	n := map[string]int{}
	for _, e := range a {
		n[e]++
	}
	for _, e := range b {
		n[e]--
	}
	var out []string
	for e, c := range n {
		if c != 0 {
			out = append(out, fmt.Sprintf("%s %+d", e, c))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// FuzzBasePathRender renders a member's pages under /m/eu through the real
// templates, from a store whose project paths, GitLab instance (dot
// segments too), branch, commit title, job, stage and image names and
// GitLab address are the fuzzer's. Invariants, on every page:
//   - no page fails (500);
//   - the only scripts are the console's own static files, with no inline
//     code; no on* attribute; no iframe, object, embed or base element; no
//     http-equiv; no style attribute that loads or runs anything;
//   - every href, src, action, data-url, data-to is an anchor, an http(s)
//     address, or under the base and still under it once a browser
//     resolves its dot segments ("%2e" and backslashes too), or a link to
//     the hub's own pages (a "/~/" link rewritten to the root) as written;
//     the base is never doubled and no hub link is moved under it;
//   - a redirect (the image of another project) goes to a same-origin path
//     under the base;
//   - the search index's addresses are under the base, resolved too;
//   - the page has exactly the elements of its twin (every '<', '>',
//     quote and '&' of the strings replaced): nothing was injected.
func FuzzBasePathRender(f *testing.F) {
	type seed struct {
		s     hfStrings
		flags uint8
	}
	real := hfStrings{"0a1b2c", "group/app", "main", "Fix the build", "build-image", "build", "https://gitlab.example.com/group/app/-/jobs/9", "app"}
	with := func(change func(*hfStrings)) hfStrings { s := real; change(&s); return s }
	seeds := []seed{
		{real, 0}, {real, 1 | 4 | 8}, {real, 2}, {real, 3},
		{hfStrings{}, 0},
		// Known defect: a member's instance made of dot segments leaves the
		// base through the project switch, pipeline, job and image links.
		{with(func(s *hfStrings) { s.Instance = "../../.." }), 0},
		{with(func(s *hfStrings) { s.Instance = ".." }), 1},
		{with(func(s *hfStrings) { s.Instance = "../../../m/us" }), 0},
		{with(func(s *hfStrings) { s.Instance = "%2e%2e/%2e%2e/%2E%2E" }), 0},
		{with(func(s *hfStrings) { s.Instance = "..\\..\\.." }), 0},
		{with(func(s *hfStrings) { s.Instance = "./../~" }), 0},
		{with(func(s *hfStrings) { s.Instance = "~" }), 0},
		// Markup and addresses in every string.
		{with(func(s *hfStrings) { s.Title = "<script>alert(1)</script>" }), 0},
		{with(func(s *hfStrings) { s.Job = `" onmouseover="alert(1)` }), 1},
		{with(func(s *hfStrings) { s.Stage = `' onclick='alert(1)` }), 0},
		{with(func(s *hfStrings) { s.Path = `g/<img src=x onerror=alert(1)>` }), 0},
		{with(func(s *hfStrings) { s.Branch = `"><iframe src="//evil.example">` }), 0},
		{with(func(s *hfStrings) { s.URL = "javascript:alert(1)" }), 0},
		{with(func(s *hfStrings) { s.URL = "data:text/html,<script>alert(1)</script>" }), 0},
		{with(func(s *hfStrings) { s.URL = "vbscript:msgbox(1)" }), 0},
		{with(func(s *hfStrings) { s.URL = "//evil.example/x" }), 0},
		{with(func(s *hfStrings) { s.URL = `https://gitlab.example.com/"><script>alert(1)</script>` }), 0},
		{with(func(s *hfStrings) { s.Image = "../../../access" }), 0},
		{with(func(s *hfStrings) { s.Image = "/~/access" }), 0},
		{with(func(s *hfStrings) { s.Image = "//evil.example" }), 0},
		{with(func(s *hfStrings) { s.Image = "" }), 0},
		{with(func(s *hfStrings) { s.Title, s.Job = "/~/", `="/x` }), 0},
		{with(func(s *hfStrings) { s.Title, s.Path = "\xff\xfe\x00", "\u202egnp.exe" }), 0},
	}
	for _, s := range seeds {
		f.Add(s.s.Instance, s.s.Path, s.s.Branch, s.s.Title, s.s.Job, s.s.Stage, s.s.URL, s.s.Image, s.flags)
	}
	members := hfMembers()
	f.Fuzz(func(t *testing.T, instance, path, branch, title, job, stage, webURL, image string, flags uint8) {
		s := hfStrings{instance, path, branch, title, job, stage, webURL, image}
		hfWithin(t, "member pages", func() []string {
			got, err := hfMemberPages(hfBackendOf(s, flags))
			if err != nil {
				return []string{err.Error()}
			}
			twin, err := hfMemberPages(hfBackendOf(s.twin(), flags))
			if err != nil {
				return []string{err.Error()}
			}
			var errs []string
			for i, p := range got {
				bad := func(format string, a ...any) {
					errs = append(errs, p.name+" "+p.path+": "+fmt.Sprintf(format, a...))
				}
				if p.code >= 500 {
					bad("status %d: %s", p.code, short(p.body, 200))
					continue
				}
				if p.code != twin[i].code {
					bad("status %d, its twin's %d", p.code, twin[i].code)
				}
				if p.code >= 300 && p.code < 400 {
					if why := hfBadLocation(p.loc, "/m/eu"); why != "" {
						bad("redirect to %q: %s", p.loc, why)
					}
					continue
				}
				if !p.html {
					var idx struct{ Items []struct{ URL string } }
					if err := json.Unmarshal([]byte(p.body), &idx); err != nil {
						bad("not JSON: %v", err)
					}
					for _, it := range idx.Items {
						if !hfUnder(hfPathOf(it.URL), "/m/eu") || !hfUnder(hfResolve(hfPathOf(it.URL)), "/m/eu") {
							bad("an address outside the base: %q", it.URL)
						}
					}
					continue
				}
				elems, perrs := hfCheckPage(p.body, p.path, "/m/eu", members)
				for _, e := range perrs {
					bad("%s", e)
				}
				twinElems, _ := hfCheckPage(twin[i].body, twin[i].path, "/m/eu", members)
				if d := hfElemDiff(elems, twinElems); d != "" {
					bad("elements differ from the twin's (injected?): %s", d)
				}
			}
			return errs
		})
	})
}

// hfSigner is sign-in through GitLab as the pages see it: one GitLab;
// Begin fails (back to the sign-in page), Finish gives back where the
// sign-in was to lead, made safe as the real sign-in stores it; the
// person may read the projects in read.
type hfSigner struct {
	read   map[uint64]bool
	to     string
	denied bool
}

func (s *hfSigner) PublicURL() string { return "https://hub.example.com" }
func (s *hfSigner) Instances() []auth.Choice {
	return []auth.Choice{{Instance: "0a1b2c", Host: "gitlab.example.com"}}
}
func (s *hfSigner) Begin(http.ResponseWriter, *http.Request, string, string) error {
	return errors.New("GitLab is away")
}
func (s *hfSigner) Finish(http.ResponseWriter, *http.Request, string) (string, error) {
	if s.denied {
		return auth.SafePath(s.to), auth.ErrDenied
	}
	return auth.SafePath(s.to), nil
}
func (s *hfSigner) Load(http.ResponseWriter, *http.Request) (*auth.Viewer, error) { return nil, nil }
func (s *hfSigner) Check(*http.Request) (*auth.Viewer, error)                     { return nil, nil }
func (s *hfSigner) SignOut(http.ResponseWriter, *http.Request) error              { return nil }
func (s *hfSigner) CanRead(_ context.Context, _ *auth.Viewer, _ string, project uint64) (bool, error) {
	return s.read[project], nil
}

// hfServe sends one request to h; target is parsed as a server parses a
// request line, and an address no server would accept is skipped (nil).
func hfServe(h http.Handler, method, target string, change func(*http.Request)) *httptest.ResponseRecorder {
	u, err := url.ParseRequestURI(target)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hfBound)
	defer cancel()
	r := httptest.NewRequest(method, "/", nil).WithContext(ctx)
	r.URL, r.RequestURI = u, target
	if change != nil {
		change(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// FuzzRedirectTargets sends every redirect made from what a request
// carries: auth.SafePath itself, the sign-in's "to" (sign-in, a failed
// start, the callback, denied or not), the project switch's "?to=" under
// a base, the hub's job and pipeline addresses (their GitLab instance and
// query), back() after a change of a cluster (its name from the address,
// with the flash message round-tripped), and the image page's redirect
// to the project that has the image, followed through the project switch.
// Invariants: every Location is a same-origin path (one leading "/", not
// "//" or "/\", no scheme or host, no control character); the sign-in
// carries SafePath's answer on; a member's redirect (and the hub's
// redirect into a member) is under that member's base, resolved too; the
// flash message comes back as it was left.
func FuzzRedirectTargets(f *testing.F) {
	for _, s := range []struct {
		to, name, query string
		flags           uint8
	}{
		{"/pipelines", "eu", "", 0},
		{"/images/app", "0a1b2c", "line=40", 1},
		{"//evil.example", "..", "to=//evil.example", 0},
		{"/\\evil.example", "%2e%2e", "\\\\evil.example", 1},
		{"\\\\evil.example", "../../..", "a=b&c=%2F%2Fevil", 0},
		{"https://evil.example/", "\\evil", "", 0},
		{"javascript:alert(1)", "a?b", "#x", 1},
		{"/auth/sign-in?to=//evil", "x#y", "", 0},
		{"/%2F%2Fevil.example", "/evil", "", 0},
		{"/..//evil.example", "//evil", "", 0},
		{"/./\\evil", "\r\nSet-Cookie: x=1", "", 0},
		{"/\t/evil", "\x00", "", 0},
		{"/\u202eevil", "~", "", 0},
		{"/a?b=//evil#//x", "/~/access", "", 0},
		{"%2e%2e", "", "", 0},
		{"../../../", "eu/../../evil", "", 1},
		{"/images/../../../access", "\u00e9", "", 0},
		{"", "eu", "", 0},
		// Control characters after "#": url.Parse lets them through in
		// a fragment.
		{"/#\x19", "%", "", 0},
		{"/x#\u202e", "eu", "", 0},
	} {
		f.Add(s.to, s.name, s.query, s.flags)
	}
	f.Fuzz(func(t *testing.T, to, name, query string, flags uint8) {
		hfWithin(t, "redirects", func() []string {
			var errs []string
			check := func(what string, rec *httptest.ResponseRecorder, base string, want ...int) {
				if rec == nil {
					return
				}
				loc := rec.Header().Get("Location")
				if rec.Code < 300 || rec.Code >= 400 {
					if loc != "" {
						errs = append(errs, fmt.Sprintf("%s: status %d with Location %q", what, rec.Code, loc))
					}
					if len(want) > 0 {
						errs = append(errs, fmt.Sprintf("%s: status %d, want a redirect", what, rec.Code))
					}
					return
				}
				if why := hfBadLocation(loc, base); why != "" {
					errs = append(errs, fmt.Sprintf("%s: Location %q: %s", what, loc, why))
				}
			}

			// SafePath: what it lets through is a same-origin path.
			sp := auth.SafePath(to)
			if why := hfBadLocation(sp, ""); why != "" {
				errs = append(errs, fmt.Sprintf("SafePath(%q) = %q: %s", to, sp, why))
			}
			rec := httptest.NewRecorder()
			http.Redirect(rec, httptest.NewRequest("GET", "/auth/callback/0a1b2c", nil), sp, http.StatusFound)
			check("a redirect to SafePath", rec, "", 0)

			// The hub's root: sign-in, and the addresses that name no
			// cluster.
			signer := &hfSigner{to: to, denied: flags&1 != 0, read: map[uint64]bool{7: true}}
			hubFleet := &hfFleet{members: hfMembers(), hits: []FleetHit{{Member: "eu",
				Pipeline: &store.Pipeline{Instance: "0a1b2c", ID: 7, ProjectID: 7},
				Job:      &store.JobInfo{Instance: "0a1b2c", ID: 7, ProjectID: 7, Node: "n1"}}}}
			hub, err := New(Empty{}, hfLog, Options{Fleet: hubFleet, Auth: signer, Version: "test"})
			if err != nil {
				return []string{err.Error()}
			}
			hub.now = func() time.Time { return testNow }
			hubMux := http.NewServeMux()
			hub.Register(hubMux)
			signed := func(r *http.Request) {
				*r = *r.WithContext(auth.With(r.Context(), &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example.com", UserID: 3, Username: "ana"}))
			}
			rec = hfServe(hubMux, "GET", "/auth/sign-in?to="+url.QueryEscape(to), nil)
			check("sign-in", rec, "", 0)
			if u, err := url.Parse(rec.Header().Get("Location")); err == nil && rec.Code == http.StatusFound {
				if got := u.Query().Get("to"); got != sp {
					errs = append(errs, fmt.Sprintf("sign-in: carries to=%q on, SafePath says %q", got, sp))
				}
			}
			check("sign-in start failed", hfServe(hubMux, "GET", "/auth/sign-in/0a1b2c?to="+url.QueryEscape(to), nil), "", 0)
			check("sign-in callback", hfServe(hubMux, "GET", "/auth/callback/0a1b2c", nil), "", 0)

			// The job and pipeline addresses printed in job logs: the one
			// cluster that has it.
			seg := url.PathEscape(name)
			q := ""
			if query != "" {
				q = "?" + query
			}
			for _, p := range []string{"/jobs/" + seg + "/7" + q, "/pipelines/" + seg + "/7" + q} {
				rec := hfServe(hubMux, "GET", p, signed)
				base := ""
				if rec != nil && rec.Code == http.StatusFound {
					// The handler's own redirect, not the mux cleaning
					// the address.
					base = "/m/eu"
				}
				check(p, rec, base)
			}

			// back() after a change of a cluster named in the address.
			rec = httptest.NewRecorder()
			hub.back(rec, httptest.NewRequest("POST", "/clusters/x/join", nil), "/clusters/"+name, to, flags&2 != 0)
			check("back", rec, "", 0)
			fr := httptest.NewRequest("GET", "/", nil)
			for _, c := range rec.Result().Cookies() {
				fr.AddCookie(c)
			}
			if msg, bad := hub.flash(httptest.NewRecorder(), fr); msg != to || bad != (flags&2 != 0) {
				if len(to) > 0 { // an empty message is no message
					errs = append(errs, fmt.Sprintf("flash: left %q (bad %v), read %q (bad %v)", to, flags&2 != 0, msg, bad))
				}
			}

			// A member's pages at /m/eu: the project switch, and an image
			// of the other project.
			b := hfBackendOf(hfStrings{Instance: "0a1b2c", Path: "group/app", Image: to}, 0)
			member, err := New(b, hfLog, Options{Base: "/m/eu", Fleet: &hfFleet{current: "eu", members: hfMembers()}, Version: "test"})
			if err != nil {
				return []string{err.Error()}
			}
			member.now = func() time.Time { return testNow }
			inner := http.NewServeMux()
			member.Register(inner)
			outer := http.NewServeMux()
			outer.Handle("/m/eu/", http.StripPrefix("/m/eu", inner))
			check("project switch", hfServe(outer, "GET", "/m/eu/projects/0a1b2c/7?to="+url.QueryEscape(to), nil), "/m/eu", 0)
			rec = hfServe(outer, "GET", "/m/eu/images/"+url.PathEscape(imageKey(to)), nil)
			if rec != nil && rec.Code == http.StatusFound {
				loc := rec.Header().Get("Location")
				check("image elsewhere", rec, "/m/eu")
				if hfBadLocation(loc, "/m/eu") == "" {
					check("image elsewhere, then the switch", hfServe(outer, "GET", loc, nil), "/m/eu")
				}
			} else {
				check("image elsewhere", rec, "")
			}
			return errs
		})
	})
}

// Markers of what only some may see on the hub's own pages.
const (
	hfLabelMark   = "LABELMARK"
	hfSubjectMark = "gitlab.example.com/77"
	hfTokenMark   = "tokmark"
	hfSecretMark  = "SECRET-P"
)

var hfRoles = []struct {
	name string
	p    fleet.Principal
}{
	{"nobody", fleet.Principal{}},
	{"viewer", fleet.Principal{Subject: "gitlab:gitlab.example.com/3", Role: fleet.Viewer}},
	{"operator", fleet.Principal{Subject: "gitlab:gitlab.example.com/2", Role: fleet.Operator}},
	{"owner", fleet.Principal{Subject: "gitlab:gitlab.example.com/1", Role: fleet.Owner}},
}

// hfMemberLink: a link or redirect into a member's job or pipeline.
var hfMemberLink = regexp.MustCompile(`/m/([^/"?#]+)/(?:jobs|pipelines)/`)

// FuzzFleetPagesAccess opens the hub's own pages (the grouped pipeline, a
// job's address, Members, a cluster, Access, its tokens, Audit) as a
// person with a fuzzed role who may read a fuzzed set of projects, while
// the members answer with fuzzed hits for one pipeline of one project
// whose title, path and job names carry that project's marker.
// Invariants:
//   - a person who may not read the project sees none of its markers, in
//     the body or the Location, and is sent to no member for it;
//   - every link or redirect into a member's job or pipeline names a
//     member whose answer the person may read (for a job: the job's
//     project; for a pipeline: the pipeline's);
//   - nobody without a role (no binding, or viewer) gets Members, Access,
//     tokens, Audit or a cluster's details: none of their markers (labels,
//     subjects, token names, fingerprints, addresses, who added a
//     cluster), and Access, Audit and the cluster answer 403;
//   - positive controls: an operator sees the fingerprints on Members and
//     the label on Access, but no token; an owner sees the token;
//   - no page fails (500).
func FuzzFleetPagesAccess(f *testing.F) {
	ctx := context.Background()
	reg, err := fleet.OpenRegistry(ctx, f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { reg.Close() })
	if err := reg.SyncOwners(ctx, []string{"gitlab:gitlab.example.com/1"}); err != nil {
		f.Fatal(err)
	}
	if _, err := reg.Grant(ctx, fleet.OriginLocal, fleet.OriginLocal, "gitlab:"+hfSubjectMark, fleet.Operator); err != nil {
		f.Fatal(err)
	}
	if err := reg.NoteName(ctx, "gitlab:"+hfSubjectMark, hfLabelMark); err != nil {
		f.Fatal(err)
	}
	if _, _, err := reg.IssueToken(ctx, fleet.OriginLocal, hfTokenMark, fleet.Operator, 24*time.Hour); err != nil {
		f.Fatal(err)
	}
	for _, s := range []struct {
		project, read, role uint8
		shape               uint32
	}{
		{1, 0, 1, 0xffffffff}, {1, 1, 1, 0xffffffff}, {2, 1, 1, 0x0f0f0f0f}, {3, 0xff, 3, 0x12345678},
		{1, 0, 0, 0}, {4, 0x07, 2, 0x00ff00ff}, {2, 0x02, 1, 0x1}, {1, 0x01, 1, 0x40000000},
	} {
		f.Add(s.project, s.read, s.role, s.shape)
	}
	f.Fuzz(func(t *testing.T, project, readMask, roleN uint8, shape uint32) {
		hfWithin(t, "hub pages", func() []string {
			p := uint64(project%4) + 1
			role := hfRoles[int(roleN)%len(hfRoles)]
			read := map[uint64]bool{}
			for i := uint64(1); i <= 4; i++ {
				read[i] = readMask&(1<<(i-1)) != 0
			}
			mark := fmt.Sprintf("%s%d-", hfSecretMark, p)
			// Each member's answer: 10 bits of shape.
			var hits []FleetHit
			var silent []string
			readsJob, readsPipe := map[string]bool{}, map[string]bool{}
			for i, m := range []string{"eu", "us", "ap"} {
				bits := shape >> (10 * i)
				h := FleetHit{Member: m}
				pid := p
				if bits&1 != 0 {
					pid = 0 // the member does not know the project
				}
				if bits&2 != 0 {
					h.Pipeline = &store.Pipeline{Instance: "0a1b2c", ID: 50, ProjectID: pid, ProjectPath: "secret/" + mark + "path",
						CommitTitle: mark + "title", Ref: mark + "ref"}
				}
				for k := 0; k < int(bits>>2&3); k++ {
					j := store.JobInfo{Instance: "0a1b2c", ID: uint64(7 + k), PipelineID: 50, ProjectID: pid, Name: fmt.Sprintf("%sjob%d", mark, k),
						Stage: mark + "stage", Status: "success", URL: "https://gitlab.example.com/" + mark + "url/-/jobs/7"}
					if bits&(16<<k) != 0 {
						j.Node = "n1"
					}
					h.Jobs = append(h.Jobs, j)
				}
				if bits&64 != 0 {
					j := store.JobInfo{Instance: "0a1b2c", ID: 7, ProjectID: pid, Name: mark + "thejob"}
					if bits&128 != 0 {
						j.PodCreated = testNow
					}
					h.Job = &j
				}
				if bits&256 != 0 {
					silent = append(silent, m)
				}
				if bits&512 != 0 {
					continue // no answer from it
				}
				hits = append(hits, h)
				if h.Job != nil && h.Job.ProjectID != 0 && read[h.Job.ProjectID] {
					readsJob[m] = true
				}
				if hp := hitProject(h); (h.Pipeline != nil || len(h.Jobs) > 0) && hp != 0 && read[hp] {
					readsPipe[m] = true
				}
			}
			srv, err := New(Empty{}, hfLog, Options{Fleet: &hfFleet{members: hfMembers(), hits: hits, silent: silent}, Auth: &hfSigner{read: read},
				Hub: &Hub{Registry: reg, Who: func(*http.Request) fleet.Principal { return role.p }, GitLabs: []string{"gitlab.example.com"}}, Version: "test"})
			if err != nil {
				return []string{err.Error()}
			}
			srv.now = func() time.Time { return testNow }
			mux := http.NewServeMux()
			srv.Register(mux)
			signed := func(r *http.Request) {
				*r = *r.WithContext(auth.With(r.Context(), &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example.com", UserID: 3, Username: "ana"}))
			}
			var errs []string
			get := func(path string) (int, string, string) {
				rec := hfServe(mux, "GET", path, signed)
				if rec.Code >= 500 {
					errs = append(errs, fmt.Sprintf("%s as %s: status %d", path, role.name, rec.Code))
				}
				return rec.Code, rec.Header().Get("Location"), rec.Body.String()
			}
			for _, c := range []struct {
				path  string
				reads map[string]bool
			}{{"/pipelines/0a1b2c/50", readsPipe}, {"/jobs/0a1b2c/7", readsJob}} {
				code, loc, body := get(c.path)
				all := loc + "\n" + body
				if !read[p] && strings.Contains(all, hfSecretMark) {
					errs = append(errs, fmt.Sprintf("%s as %s: shows project %d, which they may not read (status %d, Location %q)", c.path, role.name, p, code, loc))
				}
				for _, m := range hfMemberLink.FindAllStringSubmatch(all, -1) {
					if !c.reads[m[1]] {
						errs = append(errs, fmt.Sprintf("%s as %s: leads to member %s, whose answer they may not read", c.path, role.name, m[1]))
					}
				}
				if len(c.reads) == 0 && code != http.StatusNotFound {
					errs = append(errs, fmt.Sprintf("%s as %s: status %d with nothing they may read, want 404", c.path, role.name, code))
				}
			}
			admin := []string{hfLabelMark, hfSubjectMark, hfTokenMark, hfPrintMark, hfRemoteMark, hfAddedMark}
			for _, path := range []string{"/", "/access", "/access/tokens", "/audit", "/clusters/eu"} {
				code, _, body := get(path)
				switch role.name {
				case "nobody", "viewer":
					for _, m := range admin {
						if strings.Contains(body, m) {
							errs = append(errs, fmt.Sprintf("%s as %s: shows %q", path, role.name, m))
						}
					}
					if path != "/" && code != http.StatusForbidden {
						errs = append(errs, fmt.Sprintf("%s as %s: status %d, want 403", path, role.name, code))
					}
				case "operator":
					if path == "/access/tokens" && (code != http.StatusForbidden || strings.Contains(body, hfTokenMark)) {
						errs = append(errs, fmt.Sprintf("%s as operator: status %d, want 403 and no token", path, code))
					}
					if path == "/" && !strings.Contains(body, hfPrintMark) || path == "/access" && !strings.Contains(body, hfLabelMark) {
						errs = append(errs, fmt.Sprintf("%s as operator: the page does not show what an operator reads (status %d)", path, code))
					}
				case "owner":
					if path == "/access/tokens" && !strings.Contains(body, hfTokenMark) {
						errs = append(errs, fmt.Sprintf("%s as owner: no token listed (status %d)", path, code))
					}
				}
			}
			return errs
		})
	})
}

// hfPct reads a percentage the pages write into a style attribute.
func hfPct(what, v string) string {
	if v == "" {
		return ""
	}
	x, err := strconv.ParseFloat(v, 64)
	switch {
	case err != nil:
		return fmt.Sprintf("%s %q: not a number", what, v)
	case math.IsNaN(x) || math.IsInf(x, 0):
		return fmt.Sprintf("%s %q: not finite", what, v)
	case x < 0 || x > 100.005:
		return fmt.Sprintf("%s %s: outside [0,100]", what, v)
	}
	return ""
}

// hfTicks checks an axis' ticks: at most maxTicks+1, their positions
// percentages that never go back.
func hfTicks(what string, ts []Tick) []string {
	var errs []string
	if len(ts) > maxTicks+1 {
		errs = append(errs, fmt.Sprintf("%s: %d ticks", what, len(ts)))
	}
	last := -1.0
	for _, tk := range ts {
		if e := hfPct(what+" tick", tk.L); e != "" {
			errs = append(errs, e)
			continue
		}
		x, _ := strconv.ParseFloat(tk.L, 64)
		if x < last {
			errs = append(errs, fmt.Sprintf("%s: tick at %s after one at %.2f", what, tk.L, last))
		}
		last = x
	}
	return errs
}

// FuzzTicks lays out a pipeline's jobs on the time axes of the pipeline
// card (the pipelines list) and of the pipeline's waterfall, and an axis'
// ticks, from instants and durations a member sends: any instant, any
// order, a job's end before its start, waits longer than the pipeline, a
// clock ahead of the hub's. Invariants: each ends within the bound; at
// most maxTicks+1 ticks, at finite, non-decreasing positions in [0,100];
// on the card every job is counted once (Jobs), lanes are at most the
// stages, rows at most laneRows, and every left, width, wait-left and
// wait-width is a finite percentage in [0,100]; the waterfall has a row
// per job and the pipeline, a span longer than zero and not shorter than
// the jobs' unless the axis is cut at a year or more (no overflow), and
// every bar's left and width in [0,100].
func FuzzTicks(f *testing.F) {
	day, year := int64(24*time.Hour), int64(365*24*time.Hour)
	sec := int64(time.Second)
	now := testNow.UnixNano()
	for _, s := range []struct {
		base, a, b, c, d, e, g int64
		flags                  uint16
	}{
		// A pipeline as it runs: two jobs, a wait for a runner.
		{now - 600*sec, 0, 41 * sec, 3 * sec, 50 * sec, 20 * sec, -5 * sec, 1 << 4},
		{now - 600*sec, 0, 41 * sec, 3 * sec, 50 * sec, 20 * sec, -5 * sec, 0x2aaa},
		// Known defect: the waterfall's room for labels (span*118/100)
		// overflows past about 2.47 years.
		{now - 4*year, 0, 3 * year, 0, sec, sec, 0, 0},
		{0, 0, math.MaxInt64, 0, 0, math.MaxInt64, 0, 0},
		// Known defect: the card's positions are not clamped: a job that
		// started after the axis' end (its end before its start, or a
		// running job on a clock ahead of the hub's).
		{now - 600*sec, 0, 10 * sec, 0, 100 * sec, -95 * sec, 0, 0},
		{now - 600*sec, 0, 10 * sec, 15 * sec, 20 * sec, 0, 0, 1<<9 | 1<<11 | 1<<13 | 1<<14},
		// Waits longer than everything, extreme instants.
		{now, 0, sec, math.MaxInt64, sec, sec, math.MaxInt64, 1<<4 | 1<<10},
		{math.MinInt64, math.MaxInt64, math.MaxInt64, math.MinInt64, math.MinInt64, math.MaxInt64, math.MinInt64, 0xffff},
		{math.MaxInt64, math.MinInt64, math.MinInt64, math.MaxInt64, math.MaxInt64, math.MinInt64, math.MaxInt64, 0x5555},
		{0, 0, 0, 0, 0, 0, 0, 0},
		{now, -day, 2 * day, 0, -2 * day, day, 0, 1 << 13},
	} {
		f.Add(s.base, s.a, s.b, s.c, s.d, s.e, s.g, s.flags)
	}
	stages := []string{"build", "test", "", "deploy"}
	statuses := []string{"success", "running", "failed", ""}
	pipeStatuses := []string{"success", "running", "failed", "pending"}
	f.Fuzz(func(t *testing.T, base, a, b, c, d, e, g int64, flags uint16) {
		at := func(off int64) time.Time { return time.Unix(0, base).Add(time.Duration(off)) }
		bit := func(n uint) bool { return flags&(1<<n) != 0 }
		j1 := store.JobInfo{Instance: "0a1b2c", ID: 1, PipelineID: 50, Name: "one", Stage: stages[flags&3], Status: statuses[flags>>5&3]}
		if !bit(2) {
			j1.Started = at(a)
		}
		if !bit(3) {
			j1.Finished = at(a).Add(time.Duration(b))
		}
		if bit(4) {
			j1.Waited = time.Duration(c)
		}
		j2 := store.JobInfo{Instance: "0a1b2c", ID: 2, PipelineID: 50, Name: "two", Stage: stages[flags>>7&3], Status: statuses[flags>>11&3], Started: at(d)}
		if !bit(9) {
			j2.Finished = at(d).Add(time.Duration(e))
		}
		if bit(10) {
			j2.Waited = time.Duration(g)
		}
		j3 := store.JobInfo{Instance: "0a1b2c", ID: 3, PipelineID: 50, Name: "three", Stage: j1.Stage, Status: "success", Started: at(e), Finished: at(g)}
		jobs := []store.JobInfo{j1, j2, j3}
		p := store.PipeRow{Instance: "0a1b2c", ID: 50, Status: pipeStatuses[flags>>13&3]}
		if bit(15) {
			p.Created = at(g)
		}
		hubNow := at(d)
		if bit(14) {
			hubNow = at(c)
		}
		hfWithin(t, "time axes", func() []string {
			var errs []string
			errs = append(errs, hfTicks("ticksFor", ticksFor(time.Duration(a)))...)
			errs = append(errs, hfTicks("ticksFor", ticksFor(at(b).Sub(at(a))))...)
			srv, err := New(Empty{}, hfLog, Options{})
			if err != nil {
				return []string{err.Error()}
			}
			srv.now = func() time.Time { return hubNow }
			card := srv.pipeCard(p, jobs, nil, time.UTC)
			if card.Jobs != len(jobs) {
				errs = append(errs, fmt.Sprintf("card: %d jobs counted of %d", card.Jobs, len(jobs)))
			}
			errs = append(errs, hfTicks("card", card.Ticks)...)
			distinct := map[string]bool{}
			for _, j := range jobs {
				distinct[j.Stage] = true
			}
			if len(card.Lanes) > len(distinct) {
				errs = append(errs, fmt.Sprintf("card: %d lanes for %d stages", len(card.Lanes), len(distinct)))
			}
			placed := 0
			for _, l := range card.Lanes {
				if l.Rows > laneRows {
					errs = append(errs, fmt.Sprintf("card: lane %q has %d rows", l.Stage, l.Rows))
				}
				for _, lj := range l.Jobs {
					placed++
					for _, x := range [][2]string{{"left", lj.L}, {"width", lj.W}, {"wait left", lj.QL}, {"wait width", lj.QW}} {
						if e := hfPct("card "+x[0], x[1]); e != "" {
							errs = append(errs, e+" ("+lj.Title+")")
						}
					}
				}
			}
			if placed > len(jobs) {
				errs = append(errs, fmt.Sprintf("card: %d bars for %d jobs", placed, len(jobs)))
			}
			created := time.Time{}
			if bit(15) {
				created = at(g)
			}
			ctx, cancel := context.WithTimeout(context.Background(), hfBound)
			defer cancel()
			wf, _, cp, _, err := srv.pipelineWaterfall(ctx, "pipeline", created, nil, jobs, p.Status == "running")
			if err != nil {
				return append(errs, "waterfall: "+err.Error())
			}
			if len(cp.Names) > len(jobs) {
				errs = append(errs, fmt.Sprintf("critical path: %d jobs of %d", len(cp.Names), len(jobs)))
			}
			if wf == nil {
				return errs
			}
			if len(wf.Rows) != wf.Jobs+1 || wf.Jobs > len(jobs) {
				errs = append(errs, fmt.Sprintf("waterfall: %d rows for %d jobs", len(wf.Rows), wf.Jobs))
			}
			// The axis covers the jobs drawn: from its start (the pipeline
			// row's bar starts at 0) to the last end.
			if wf.Span <= 0 {
				errs = append(errs, fmt.Sprintf("waterfall: span %v", wf.Span))
			}
			errs = append(errs, hfTicks("waterfall", wf.Ticks)...)
			for _, r := range wf.Rows {
				for _, bar := range r.Bars {
					if e := hfPct("waterfall left", bar.L); e != "" {
						errs = append(errs, e+" (row "+r.Name+")")
					}
					if e := hfPct("waterfall width", bar.W); e != "" {
						errs = append(errs, e+" (row "+r.Name+")")
					}
				}
			}
			// No overflow: the axis is the pipeline's time and room for the
			// labels (100/118 of it is the pipeline's bar), or a pipeline
			// that took no time (the narrowest bar). Spans of nanoseconds
			// round too coarsely to say.
			if len(wf.Rows) > 0 && len(wf.Rows[0].Bars) > 0 && wf.Span >= time.Microsecond {
				// An axis cut at a year or more draws a longer pipeline to
				// the edge.
				pw, err := strconv.ParseFloat(wf.Rows[0].Bars[0].W, 64)
				cut := pw == 100 && wf.Span >= 365*24*time.Hour
				if err != nil || pw != 0.25 && !cut && (pw < 84.5 || pw > 85.0) {
					errs = append(errs, fmt.Sprintf("waterfall: the pipeline's bar is %s%% of an axis of %v (an overflow?)", wf.Rows[0].Bars[0].W, wf.Span))
				}
			}
			return errs
		})
	})
}
