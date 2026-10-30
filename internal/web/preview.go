package web

import (
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Link previews: chat apps and social sites show a link as a card made of
// the page's Open Graph tags (a title, a sentence, an image). Their
// fetchers are signed in to nothing, so with sign-in on they get the
// sign-in page and its card, never a project's builds.

// PublicURLEnv names the variable holding where browsers reach the
// console, for the absolute addresses the tags need. Without it the
// request's own host is used.
const PublicURLEnv = "CI_ACCELERATOR_PUBLIC_URL"

// siteDesc describes the console when a page has nothing of its own.
const siteDesc = "A self-hosted build farm for GitLab: why each image build was slow or not cached - the code or the farm."

var hostRE = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)

// Origin is "scheme://host" of an http(s) URL, or "" for anything else.
func Origin(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || !hostRE.MatchString(p.Host) || p.User != nil {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

// origin is where the browser reached the console: the configured address,
// else the request's host (behind a proxy, the scheme it says it served).
func (s *Server) origin(r *http.Request) string {
	if s.opt.PublicURL != "" {
		return s.opt.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); strings.TrimSpace(p) == "https" || strings.TrimSpace(p) == "http" {
		scheme = strings.TrimSpace(p)
	}
	if !hostRE.MatchString(r.Host) {
		return ""
	}
	return scheme + "://" + r.Host
}

// PreviewTitle is the card's title: the page's, with its project.
func (sh Shell) PreviewTitle() string {
	t := sh.Preview
	if t == "" {
		t = sh.Title
	}
	if t == "" {
		t = "ci-accelerator"
	}
	if p := sh.Project.Path; p != "" && !sh.NoProjects && !strings.Contains(t, p) {
		t += " · " + p
	}
	return clipWords(t, 120)
}

// PreviewDesc is the card's sentence: the page's own, else what the
// console shows of the project.
func (sh Shell) PreviewDesc() string {
	if sh.Desc != "" {
		return clipWords(sh.Desc, 240)
	}
	if p := sh.Project.Path; p != "" && !sh.NoProjects {
		return clipWords(fmt.Sprintf("Image builds of %s on a self-hosted build farm: what came from the cache, and why the rest was rebuilt - the code or the farm.", p), 240)
	}
	return siteDesc
}

// PreviewTitle and PreviewDesc of the sign-in page say nothing of a project.
func (v SignIn) PreviewTitle() string { return "ci-accelerator console" }
func (v SignIn) PreviewDesc() string  { return siteDesc }

// previewFetchers name the user agents of chat apps and social sites
// that fetch a shared link to show its preview.
var previewFetchers = regexp.MustCompile(`(?i)TelegramBot|Slackbot|Twitterbot|facebookexternalhit|Discordbot|WhatsApp|LinkedInBot|SkypeUriPreview|Mattermost|redditbot|vkShare|Iframely`)

// previewFetcher: r fetches a link's preview. Whoever sends such a user
// agent only gets a page a browser would also get.
func previewFetcher(r *http.Request) bool { return previewFetchers.MatchString(r.UserAgent()) }

var tagRE = regexp.MustCompile(`<[^>]*>`)

// plain is the text of a sentence the pages render as HTML.
func plain(h template.HTML) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(string(h), ""))), " ")
}

// clipWords shortens s to at most n runes at a word, with an ellipsis.
func clipWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n-1])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}
