package web

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// meta is the content of a page's <meta property|name=key>, unescaped, and
// whether it is there.
func meta(body, key string) (string, bool) {
	m := regexp.MustCompile(`<meta (?:property|name)="` + regexp.QuoteMeta(key) + `" content="([^"]*)">`).FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

func getWith(t *testing.T, ts interface{ Client() *http.Client }, url string, header map[string]string) page {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return page{status: res.StatusCode, header: res.Header, body: b.String()}
}

// A shared link to any page shows as a card: a title naming the page and
// its project, a sentence of what the page says, the console's image at
// an absolute address.
func TestLinkPreviews(t *testing.T) {
	ts, _, _ := console(t)
	home := get(t, ts, "/")
	title, _ := meta(home.body, "og:title")
	if !strings.HasPrefix(title, "Pipelines · ") || len(title) <= len("Pipelines · ") {
		t.Errorf("home og:title %q: want the pipelines and the project", title)
	}
	if d, _ := meta(home.body, "og:description"); !strings.Contains(d, "Image builds of ") {
		t.Errorf("home og:description %q", d)
	}
	img, ok := meta(home.body, "og:image")
	if !ok || !strings.HasPrefix(img, ts.URL+"/static/og.png") {
		t.Errorf("og:image %q: want an absolute address on %s", img, ts.URL)
	}
	if u, _ := meta(home.body, "og:url"); u != ts.URL+"/" {
		t.Errorf("og:url %q", u)
	}
	if c, _ := meta(home.body, "twitter:card"); c != "summary_large_image" {
		t.Errorf("twitter:card %q", c)
	}
	// The image itself, as a chat app fetches it.
	if p := get(t, ts, strings.TrimPrefix(img, ts.URL)); p.status != 200 || p.header.Get("Content-Type") != "image/png" || len(p.body) < 10000 {
		t.Errorf("og.png: %d %s, %d bytes", p.status, p.header.Get("Content-Type"), len(p.body))
	}

	// A build: its name and pipeline, the verdict as plain text with the time.
	first := regexp.MustCompile(`href="(/builds/\d+)"`).FindStringSubmatch(get(t, ts, "/builds").body)
	if first == nil {
		t.Fatal("no build on /builds")
	}
	b := get(t, ts, first[1])
	bt, _ := meta(b.body, "og:title")
	bd, _ := meta(b.body, "og:description")
	if !strings.Contains(bt, "pipeline #") || bd == "" || strings.ContainsAny(bd, "<>") || !strings.Contains(bd, "The build took ") {
		t.Errorf("build preview: %q / %q", bt, bd)
	}
	// A pipeline: its number and the page's sentence.
	pl := regexp.MustCompile(`href="(/pipelines/[0-9a-f]+/\d+)"`).FindStringSubmatch(home.body)
	if pl == nil {
		t.Fatal("no pipeline on the home page")
	}
	pp := get(t, ts, pl[1])
	if pt, _ := meta(pp.body, "og:title"); !strings.HasPrefix(pt, "Pipeline #") {
		t.Errorf("pipeline og:title %q", pt)
	}
	if pd, _ := meta(pp.body, "og:description"); !strings.Contains(pd, "ready") && !strings.Contains(pd, "image") {
		t.Errorf("pipeline og:description %q", pd)
	}
	// Insights: the figures of its first sentence.
	if d, _ := meta(get(t, ts, "/insights").body, "og:description"); !strings.Contains(d, "fully from the cache") {
		t.Errorf("insights og:description %q", d)
	}
	// Every page of the crawl carries a card.
	for _, p := range []string{"/builds", "/images", "/nodes", "/nope"} {
		if _, ok := meta(get(t, ts, p).body, "og:title"); !ok {
			t.Errorf("%s has no link preview", p)
		}
	}
}

// The absolute addresses come from where the console is reached: the
// configured address, else the request (the scheme a proxy says it
// served); a host that is no host leaves them out.
func TestLinkPreviewOrigin(t *testing.T) {
	ts, _, _ := console(t)
	p := getWith(t, ts, ts.URL+"/", map[string]string{"Host": "demo.example.com", "X-Forwarded-Proto": "https"})
	if img, _ := meta(p.body, "og:image"); !strings.HasPrefix(img, "https://demo.example.com/static/og.png") {
		t.Errorf("behind a proxy: og:image %q", img)
	}
	p = getWith(t, ts, ts.URL+"/", map[string]string{"Host": `evil.example.com"><script>`})
	if p.status == 200 {
		if _, ok := meta(p.body, "og:image"); ok || strings.Contains(p.body, "evil.example.com") {
			t.Error("a forged Host made it into the page")
		}
	}
	tsp, _, _, _ := consoleWith(t, testOpts{PublicURL: "https://builds.example.com"})
	p = getWith(t, tsp, tsp.URL+"/insights", map[string]string{"Host": "other.example.com"})
	if u, _ := meta(p.body, "og:url"); u != "https://builds.example.com/insights" {
		t.Errorf("configured address: og:url %q", u)
	}
	for in, want := range map[string]string{
		"https://demo.ci-accelerator.io":   "https://demo.ci-accelerator.io",
		"https://demo.ci-accelerator.io/x": "https://demo.ci-accelerator.io",
		"http://localhost:8080/":           "http://localhost:8080",
		"javascript:alert(1)":              "",
		"ftp://example.com":                "",
		"https://user:pw@example.com":      "",
		"demo.ci-accelerator.io":           "",
	} {
		if got := Origin(in); got != want {
			t.Errorf("Origin(%q) = %q, want %q", in, got, want)
		}
	}
}

// With sign-in, a link preview never shows a project: the chat app's
// fetcher is not signed in and gets the sign-in page's card.
func TestLinkPreviewSignedOut(t *testing.T) {
	ts, _, _, _, browser := signedIn(t)
	anon := browser("")
	res, err := anon.Get(ts.URL + "/pipelines/abcdef/1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("signed out: %d", res.StatusCode)
	}
	// A browser goes on to GitLab; Telegram's fetcher stays and gets the card.
	if p := getWith(t, ts, ts.URL+res.Header.Get("Location"), nil); p.status != http.StatusFound {
		t.Errorf("a browser is not sent on to GitLab: %d", p.status)
	}
	p := getWith(t, ts, ts.URL+res.Header.Get("Location"), map[string]string{"User-Agent": "TelegramBot (like TwitterBot)"})
	if p.status != 200 {
		t.Fatalf("the preview fetcher: %d", p.status)
	}
	title, _ := meta(p.body, "og:title")
	desc, _ := meta(p.body, "og:description")
	if title != "ci-accelerator console" || desc != siteDesc || strings.Contains(p.body, "example/") {
		t.Errorf("sign-in card: %q / %q", title, desc)
	}
}

func TestClipWords(t *testing.T) {
	if got := clipWords("one two three four five", 14); got != "one two three…" {
		t.Errorf("clipWords: %q", got)
	}
	if got := plain(`Rebuilt from <code>COPY a b</code>: &lt;x&gt; changed.`); got != "Rebuilt from COPY a b: <x> changed." {
		t.Errorf("plain: %q", got)
	}
}
