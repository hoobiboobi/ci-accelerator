package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/logging"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
	"github.com/hoobiboobi/ci-accelerator/internal/web"
)

// The hub's numbers that its texts and docs quote.
const (
	// dialWait bounds a new link's first read of the member's controller.
	dialWait = 30 * time.Second
	// deliverWait bounds one delivery of a member's secrets.
	deliverWait = 30 * time.Second
	// retryFirst and retryMax: a failed delivery is tried again after
	// retryFirst, doubling up to retryMax, while the link stays up.
	retryFirst = 30 * time.Second
	retryMax   = 10 * time.Minute
	// refreshEvery: how often the hub reads the registry again (a command
	// in its pod writes the file directly) and tries due deliveries.
	refreshEvery = 15 * time.Second
	pruneEvery   = time.Hour
	// keptPages and keptBytes bound the pages kept of members that went
	// away; keptMaxAge is how long one is shown.
	keptPages  = 2000
	keptBytes  = 64 << 20
	keptMaxAge = 24 * time.Hour
	// minAPIToken is the shortest bootstrap API token taken.
	minAPIToken = 24
	// listWait bounds a read of the member list for a page.
	listWait = 5 * time.Second
	// jobGrace: once a cluster says it ran a job, how long the others
	// still have to answer.
	jobGrace = 300 * time.Millisecond
	// stopWait: how long the hub waits for requests at its stop.
	stopWait = 5 * time.Second
	// maxDeliveryAnswer bounds a member's answer to a delivery.
	maxDeliveryAnswer = 64 << 10
)

// hub is a fleet hub's state: the members connected now, each with the
// pages of its console.
type hub struct {
	root    *web.Server // the hub's own pages
	rootMux *http.ServeMux
	seen    *fleet.LastKnown
	reg     *fleet.Registry
	log     *slog.Logger
	signIn  *auth.Auth
	admin   *web.Hub
	token   *bootstrapToken
	// sealer opens the secrets the hub keeps for its clusters; nil: it
	// keeps none.
	sealer  *fleet.Sealer
	assets  string
	version string
	// dialWait overrides dialWait (tests).
	dialWait time.Duration

	mu    sync.Mutex
	links map[string]*memberLink
	// lastList: the member list as last read, shown while the registry
	// does not answer.
	lastList []fleet.Member
	// delivering serialises the deliveries to each member; retry holds
	// when a member's failed delivery is due again; forced: members that
	// got their secrets once since this hub started.
	delivering map[string]*sync.Mutex
	retry      map[string]*retryState
	forced     map[string]bool
}

type memberLink struct {
	link   *fleet.Link
	client *control.Client
	pages  http.Handler
	// ctx ends with the link: the requests (streams) to its pages end
	// with it.
	ctx  context.Context
	stop context.CancelFunc
	// gone: the member was removed, or its store replaced: what its pages
	// answer from now on is not kept.
	gone atomic.Bool
}

type retryState struct {
	wait time.Duration
	next time.Time
}

// view is the fleet as one page of the hub sees it.
type view struct {
	h       *hub
	current string
}

func (v view) Current() string { return v.current }

// list is every member as the registry has them; while the registry does
// not answer, the list as last read.
func (h *hub) list(ctx context.Context) []fleet.Member {
	ctx, cancel := context.WithTimeout(ctx, listWait)
	defer cancel()
	ms, err := h.reg.List(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.log.Warn("fleet: the member list does not answer; the one read last is shown", "err", err)
		return h.lastList
	}
	h.lastList = ms
	return ms
}

func (v view) Members() []web.FleetMember {
	ms := v.h.list(context.Background())
	v.h.mu.Lock()
	defer v.h.mu.Unlock()
	var out []web.FleetMember
	for _, m := range ms {
		fm := web.FleetMember{Name: m.Name, State: "offline", Version: m.Version, SeenAt: m.SeenAt, Fingerprint: m.Fingerprint,
			Origin: m.Origin, AddedBy: m.AddedBy, AddedAt: m.AddedAt, StoreReplaced: m.StoreReplaced, RemovedAt: m.RemovedAt}
		switch {
		case !m.RemovedAt.IsZero():
			fm.State = "removed"
		case v.h.links[m.Name] != nil && v.h.links[m.Name].pages != nil:
			l := v.h.links[m.Name].link
			fm.State, fm.Since, fm.Remote, fm.Version = "connected", l.Since, l.Remote, l.Version
		}
		out = append(out, fm)
	}
	return out
}

// Locate asks every connected member, at once, for a pipeline's jobs or a
// job; members that do not answer before ctx ends, and registered members
// not connected now, are silent. A job is answered once a member says it
// ran it and the others had jobGrace more.
func (v view) Locate(ctx context.Context, instance string, pipeline, job uint64) ([]web.FleetHit, []string) {
	ms := v.h.list(ctx)
	v.h.mu.Lock()
	clients := map[string]*control.Client{}
	for n, ml := range v.h.links {
		if ml.client != nil && ml.pages != nil {
			clients[n] = ml.client
		}
	}
	v.h.mu.Unlock()
	var silent []string
	for _, m := range ms {
		if m.RemovedAt.IsZero() && clients[m.Name] == nil {
			silent = append(silent, m.Name)
		}
	}
	type answer struct {
		hit web.FleetHit
		ok  bool
	}
	out := make(chan answer, len(clients))
	for n, c := range clients {
		go func() {
			h := web.FleetHit{Member: n}
			var err error
			if job != 0 {
				h.Job, err = c.Job(ctx, instance, job)
			} else {
				if h.Pipeline, err = c.PipelineInfo(ctx, instance, pipeline); err == nil {
					if h.Pipeline != nil && c.Config().GitLab {
						// The jobs' states as GitLab has them now, as the
						// cluster's own pipeline page reads them.
						c.Ensure(ctx, instance, h.Pipeline.ProjectID, pipeline, h.Pipeline.Status, h.Pipeline.APIAt, 700*time.Millisecond)
					}
					h.Jobs, err = c.PipelineJobs(ctx, instance, pipeline)
				}
			}
			out <- answer{h, err == nil}
		}()
	}
	var hits []web.FleetHit
	answered := map[string]bool{}
	var grace <-chan time.Time
	for len(answered) < len(clients) {
		select {
		case a := <-out:
			answered[a.hit.Member] = true
			if !a.ok {
				silent = append(silent, a.hit.Member)
				continue
			}
			hits = append(hits, a.hit)
			if grace == nil && a.hit.Job != nil && ranJob(*a.hit.Job) {
				grace = time.After(jobGrace)
			}
		case <-grace:
			for n := range clients {
				if !answered[n] {
					answered[n] = true
					silent = append(silent, n)
				}
			}
		}
	}
	sort.Strings(silent)
	return hits, silent
}

// ranJob says whether the cluster that told of a job ran it (it saw the
// job's pod), as the job's page at the hub judges it.
func ranJob(j store.JobInfo) bool { return !j.PodCreated.IsZero() || j.Node != "" }

// up takes a member's new link: its controller's client, its pages.
func (h *hub) up(l *fleet.Link) {
	ctx, cancel := context.WithCancel(context.Background())
	ml := &memberLink{link: l, ctx: ctx, stop: cancel}
	h.mu.Lock()
	old := h.links[l.Name]
	h.links[l.Name] = ml
	h.mu.Unlock()
	if old != nil {
		// A member that comes back before its old link is noticed gone
		// does so from the same address; two holders of one key take
		// turns from two.
		if host(old.link.Remote) != host(l.Remote) {
			h.log.Warn("fleet: a member's new link replaces an open one from another address: is its key used twice?",
				"name", l.Name, "remote", l.Remote, "replaced_remote", old.link.Remote, "replaced_since", old.link.Since)
		} else {
			h.log.Info("fleet: a member's new link replaces its old one", "name", l.Name, "remote", l.Remote)
		}
		old.end()
	}
	// After the link is in the map: a removal from now on finds it (drop),
	// one before is seen here.
	replaced, err := h.reg.Seen(ctx, l.Name, l.Store, l.Version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		h.log.Warn("fleet: a member was removed while it connected: its link is closed", "name", l.Name, "remote", l.Remote)
		h.unlink(ml)
		return
	case err != nil:
		h.log.Warn("fleet: recording a member's visit", "name", l.Name, "err", err)
	case replaced:
		if old != nil {
			old.gone.Store(true)
		}
		h.seen.Forget(l.Name + "\x00")
		h.log.Warn("fleet: a member came with another store: its earlier builds' addresses lead nowhere now", "name", l.Name, "store", l.Store)
	}
	go func() {
		// Every way out takes the link out of the map (if it is still this
		// one) and ends its pages' requests.
		defer h.unlink(ml)
		wait := h.dialWait
		if wait == 0 {
			wait = dialWait
		}
		dctx, dcancel := context.WithTimeout(ctx, wait)
		client, err := control.Dial(dctx, control.ClientOptions{URL: "http://" + l.Name, HTTP: l.HTTP(time.Minute), Stream: l.HTTP(0),
			Member: true, Log: h.log.With("member", l.Name)})
		dcancel()
		if err != nil {
			h.log.Warn("fleet: the member's controller did not answer", "name", l.Name, "err", err)
			return
		}
		go client.Run(ctx)
		cfg := client.Config()
		if cfg.API != control.APIVersion {
			h.log.Warn("fleet: the member's controller speaks another version of the console's API; some of its pages may not show",
				"name", l.Name, "member_api", cfg.API, "hub_api", control.APIVersion, "member_version", cfg.Version)
		}
		opt := h.memberOptions(l.Name, cfg.Release)
		if cfg.GitLab {
			// GitLab is read by the member, with the member's token.
			opt.GitLab, opt.InstallToken = client, cfg.InstallToken
		}
		pages, err := web.New(client, h.log.With("member", l.Name), opt)
		if err != nil {
			h.log.Error("fleet: the member's pages", "name", l.Name, "err", err)
			return
		}
		mux := http.NewServeMux()
		pages.Register(mux)
		h.mu.Lock()
		current := h.links[l.Name] == ml
		if current {
			ml.pages, ml.client = mux, client
		}
		// The first link since the hub started, or a new store (installed
		// anew): the cluster gets its secrets again even when delivered
		// before. Other reconnects deliver only what is not delivered yet.
		force := replaced || !h.forced[l.Name]
		h.mu.Unlock()
		if !current {
			return
		}
		go func() {
			if h.deliver(ctx, l.Name, force) && force {
				h.mu.Lock()
				h.forced[l.Name] = true
				h.mu.Unlock()
			}
		}()
		select {
		case <-l.Done():
		case <-ctx.Done():
		}
	}()
}

// unlink takes a link out of the map, if it is still the member's, ends
// its pages' requests and closes it; the member's last contact is now.
func (h *hub) unlink(ml *memberLink) {
	h.mu.Lock()
	current := h.links[ml.link.Name] == ml
	if current {
		delete(h.links, ml.link.Name)
	}
	h.mu.Unlock()
	ml.end()
	if current && !ml.gone.Load() {
		// No store: Seen keeps the one recorded. A removed member has no
		// row to update.
		h.reg.Seen(context.Background(), ml.link.Name, "", ml.link.Version)
	}
}

// host is an address without its port.
func host(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// memberOptions are a cluster's pages' options at the hub: under its base,
// with the hub's sign-in and the hub's roles (the sidebar's Fleet links and
// the role it names).
func (h *hub) memberOptions(name, release string) web.Options {
	opt := web.Options{Base: "/m/" + name, Release: release, Version: h.version, PublicURL: publicURL(h.log),
		Fleet: view{h, name}, Hub: h.admin}
	if h.assets != "" {
		opt.Assets = os.DirFS(h.assets)
	}
	if h.signIn != nil {
		opt.Auth = h.signIn
	}
	return opt
}

// status is a member's link as the API reports it: connected once its
// pages are there, as the Members page says.
func (h *hub) status(name string) fleet.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	ml := h.links[name]
	if ml == nil || ml.pages == nil {
		return fleet.Status{}
	}
	return fleet.Status{Connected: true, Since: ml.link.Since, Remote: ml.link.Remote, Protocol: ml.link.Protocol}
}

// drop closes a removed member's link and forgets its pages; a page
// request still running is not kept either.
func (h *hub) drop(name string) {
	h.mu.Lock()
	ml := h.links[name]
	if ml != nil {
		ml.gone.Store(true)
		delete(h.links, name)
	}
	h.mu.Unlock()
	h.seen.Forget(name + "\x00")
	if ml != nil {
		ml.end()
	}
}

// end ends a link's pages' requests and closes it.
func (ml *memberLink) end() {
	if ml.stop != nil {
		ml.stop()
	}
	if ml.link != nil {
		ml.link.Close()
	}
}

// member serves /m/<name>/... from that member's pages; while the member
// is away, the copy of the page the same person last opened, dated.
func (h *hub) member(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !fleet.NamePattern.MatchString(name) {
		h.root.MemberAway(w, r, name)
		return
	}
	prefix := "/m/" + name
	if esc := r.URL.EscapedPath(); !strings.HasPrefix(esc, prefix+"/") {
		// The name written escaped (/m/%65u/): to its plain address.
		rest := "/"
		if i := strings.IndexByte(esc[len("/m/"):], '/'); i >= 0 {
			rest = esc[len("/m/")+i:]
		}
		to := prefix + rest
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusPermanentRedirect)
		return
	}
	if strings.HasPrefix(r.URL.Path, prefix+"/static/") {
		// Every cluster's pages are rendered here, by this hub's own
		// templates: their script and stylesheet are the hub's, and are
		// there while the cluster is away too.
		http.StripPrefix(prefix, h.rootMux).ServeHTTP(w, r)
		return
	}
	h.mu.Lock()
	ml := h.links[name]
	var pages http.Handler
	if ml != nil {
		pages = ml.pages
	}
	h.mu.Unlock()
	// Kept per person, and apart for what they saw with every project
	// (an operator's copy is not shown once they are a viewer).
	who := "-"
	if v := auth.From(r.Context()); v != nil {
		who = v.Host + "/" + strconv.FormatUint(v.UserID, 10) + "/" + strconv.FormatBool(v.Admin)
	}
	key := name + "\x00" + who + "\x00" + r.URL.RequestURI()
	if pages == nil {
		w.Header().Set("Cache-Control", "no-store")
		// A kept copy for a page opened in the browser, not for the
		// page's own fetches (its refresh then says nothing new) nor a
		// form sent.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && opened(r) {
			if body, ct, at, ok := h.seen.Get(key); ok && time.Since(at) < keptMaxAge {
				if body, ok := withNote(body, name, at, time.Now()); ok {
					w.Header().Set("Content-Type", ct)
					w.Header().Set("X-Fleet-Last-Known", at.UTC().Format(time.RFC3339))
					w.Write(body)
					return
				}
			}
		}
		h.root.MemberAway(w, r, name)
		return
	}
	// The requests (an open page's stream) end with the link: the page
	// then reconnects to the member's next link.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if ml.ctx != nil {
		defer context.AfterFunc(ml.ctx, cancel)()
	}
	h.seen.Record(key, http.StripPrefix(prefix, pages)).ServeHTTP(w, r.WithContext(ctx))
	if ml.gone.Load() {
		// Removed (or its store replaced) while this request ran: what it
		// just kept goes too.
		h.seen.Forget(name + "\x00")
	}
}

// opened says whether a request is a page the browser opens (not a
// fetch of the page's script): browsers that do not say are taken as
// opening it.
func opened(r *http.Request) bool {
	m := r.Header.Get("Sec-Fetch-Mode")
	return m == "" || m == "navigate"
}

// pageStart is where a page's content starts: the note of a kept copy
// goes right inside it.
var pageStart = regexp.MustCompile(`<div class="page(?: [^"]*)?"[^>]*>`)

// withNote is a kept page with the note that it is a copy, put at the
// start of its content; false when the page has no place for it (it is
// then not shown as if current).
func withNote(body []byte, name string, at, now time.Time) ([]byte, bool) {
	loc := pageStart.FindIndex(body)
	if loc == nil {
		return nil, false
	}
	note := fmt.Sprintf(`<div class="callout warn" role="note"><div><b class="k">%s is not connected now.</b> This is the page as it was at %s UTC (%s); it updates again once the cluster reconnects.</div></div>`,
		html.EscapeString(name), at.UTC().Format("15:04"), agoText(now.Sub(at)))
	out := make([]byte, 0, len(body)+len(note))
	out = append(out, body[:loc[1]]...)
	out = append(out, note...)
	return append(out, body[loc[1]:]...), true
}

// agoText is an age as the pages write it: "seconds ago", "1 minute
// ago", "14 minutes ago", whole units, not rounded up.
func agoText(d time.Duration) string {
	unit := func(n int, u string) string {
		if n == 1 {
			return "1 " + u + " ago"
		}
		return strconv.Itoa(n) + " " + u + "s ago"
	}
	switch {
	case d < time.Minute:
		return "seconds ago"
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return unit(int(d/time.Hour), "hour")
	}
	return unit(int(d/(24*time.Hour)), "day")
}

// bootstrapToken is the management API's bootstrap token, read from its
// file at every use: the file follows its Secret, so a token changed or
// removed there ends without a restart.
type bootstrapToken struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	warned string
}

// configured says whether the hub was given a bootstrap token's file.
func (b *bootstrapToken) configured() bool { return b != nil && b.path != "" }

// get is the token now; "" when there is none: no file, an empty one, or
// one too short.
func (b *bootstrapToken) get() string {
	if !b.configured() {
		return ""
	}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.warn(err.Error(), "fleet: the bootstrap API token's file does not read: no bootstrap token", "err", err)
		}
		return ""
	}
	t := strings.TrimSpace(string(raw))
	if t != "" && len(t) < minAPIToken {
		b.warn("short", "fleet: the bootstrap API token is shorter than 24 characters: not taken")
		return ""
	}
	return t
}

// warn logs a problem with the file once, until it changes.
func (b *bootstrapToken) warn(what, msg string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.warned != what {
		b.warned = what
		b.log.Warn(msg, append(args, "file", b.path)...)
	}
}

// bearer is a request's bearer token (the scheme in any case).
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return h[len("Bearer "):], true
	}
	return "", false
}

// who is the principal of a request: a bearer token's, or the person
// signed in with their role at the hub.
func (h *hub) who(r *http.Request) fleet.Principal {
	if t, ok := bearer(r); ok {
		if bt := h.token.get(); bt != "" && subtle.ConstantTimeCompare([]byte(t), []byte(bt)) == 1 {
			return fleet.Principal{Subject: "token:" + fleet.BootstrapToken, Role: fleet.Owner}
		}
		p, _ := h.reg.TokenPrincipal(r.Context(), t)
		return p
	}
	v := auth.From(r.Context())
	if v == nil {
		return fleet.Principal{}
	}
	subject := fleet.PersonSubject(v.Host, v.UserID)
	label := "@" + v.Username
	if v.Name != "" {
		label = v.Name + " @" + v.Username
	}
	if err := h.reg.NoteName(r.Context(), subject, label); err != nil {
		h.log.Warn("fleet: recording a person's name", "err", err)
	}
	return fleet.Principal{Subject: subject, Role: h.reg.RoleOf(subject)}
}

// hubConfig is what newHub builds the hub from.
type hubConfig struct {
	reg     *fleet.Registry
	log     *slog.Logger
	key     crypto.Signer
	token   *bootstrapToken
	signIn  *auth.Auth
	sealer  *fleet.Sealer
	assets  string
	version string
	info    fleet.HubInfo
}

// newHub builds the hub and its whole HTTP surface: the members' links
// and joins, the health checks, the management API and the pages.
func newHub(ctx context.Context, c hubConfig) (*hub, http.Handler, error) {
	log, reg := c.log, c.reg
	h := &hub{reg: reg, log: log, signIn: c.signIn, sealer: c.sealer, token: c.token, assets: c.assets, version: c.version,
		links: map[string]*memberLink{}, seen: fleet.NewLastKnown(keptPages, keptBytes),
		delivering: map[string]*sync.Mutex{}, retry: map[string]*retryState{}, forced: map[string]bool{}}
	h.admin = &web.Hub{Registry: reg, Who: h.who, Bootstrap: c.token.configured(), Sealer: h.sealer,
		Deliver: func(name string) { go h.deliver(context.WithoutCancel(ctx), name, false) }, Removed: h.drop,
		NoOwners: func() bool {
			if h.token.get() != "" {
				return false
			}
			bctx, cancel := context.WithTimeout(ctx, listWait)
			defer cancel()
			bs, err := reg.Bindings(bctx)
			for _, b := range bs {
				if b.Role == fleet.Owner {
					return false
				}
			}
			return err == nil
		},
		Version: chartVersion(), Info: c.info}
	var signInHosts []string
	if h.signIn != nil {
		for _, ch := range h.signIn.Instances() {
			h.admin.GitLabs = append(h.admin.GitLabs, ch.Host)
			signInHosts = append(signInHosts, strings.ToLower(ch.Host))
		}
	}

	// The hub's own pages: the members and signing in.
	opt := web.Options{Version: c.version, Release: "hub", PublicURL: publicURL(log), Fleet: view{h, ""}, Hub: h.admin}
	if c.assets != "" {
		opt.Assets = os.DirFS(c.assets)
	}
	if h.signIn != nil {
		opt.Auth = h.signIn
	}
	root, err := web.New(web.Empty{}, log, opt)
	if err != nil {
		return nil, nil, err
	}
	h.root = root
	h.rootMux = http.NewServeMux()
	root.Register(h.rootMux)

	api := &fleet.API{Registry: reg, Status: h.status, Removed: h.drop, Version: c.version, Who: h.who,
		Sealer: h.sealer, Deliver: h.admin.Deliver, Hub: c.info, SignInHosts: signInHosts}
	apiMux := http.NewServeMux()
	api.Register(apiMux)

	pagesMux := http.NewServeMux()
	pagesMux.HandleFunc("/m/{name}/", h.member)
	pagesMux.HandleFunc("GET /m/{name}", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.EscapedPath() + "/"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusPermanentRedirect)
	})
	pagesMux.Handle(fleet.APIPrefix, apiMux)
	pagesMux.Handle("/", h.rootMux)
	gated := web.AccessLog(log, web.Secure(web.Compress(root.Gate(pagesMux))))
	direct := web.AccessLog(log, web.Secure(web.Compress(apiMux)))
	withToken := web.AccessLog(log, web.Secure(web.Compress(logRefused(log, "fleet: an API token was refused", apiMux,
		http.StatusUnauthorized, http.StatusForbidden))))

	link := &fleet.Hub{Key: c.key, Member: reg.Holder, Up: h.up, Log: log}
	top := http.NewServeMux()
	top.Handle("GET "+fleet.ConnectPath, link)
	top.Handle("POST "+fleet.JoinPath, web.AccessLog(log, logRefused(log, "fleet: a join was refused", reg.JoinHandler(), http.StatusForbidden)))
	top.HandleFunc("GET /healthz", h.healthz)
	top.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	top.HandleFunc(fleet.APIPrefix, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bearer(r); ok {
			// A token comes without a session: past the sign-in gate.
			withToken.ServeHTTP(w, r)
			return
		}
		if _, err := r.Cookie(auth.SessionCookie); err != nil {
			// Neither a token nor a session: the API answers itself (its
			// JSON 401), not the pages' sign-in gate.
			direct.ServeHTTP(w, r)
			return
		}
		gated.ServeHTTP(w, r)
	})
	top.Handle("/", gated)
	return h, top, nil
}

// healthz: the hub is ready while its registry answers.
func (h *hub) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := h.reg.Get(ctx, "-"); err != nil && !errors.Is(err, fleet.ErrNotFound) {
		h.log.Warn("fleet: health check: the registry does not answer", "err", err)
		http.Error(w, "the registry does not answer", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprint(w, "ok")
}

// logRefused logs the answers of h with one of codes, with the caller's
// address.
func logRefused(log *slog.Logger, msg string, h http.Handler, codes ...int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(sw, r)
		for _, c := range codes {
			if sw.code == c {
				log.Warn(msg, "status", sw.code, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			}
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.code, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// openHubRegistry opens the hub's fleet.db, after applying a restore
// staged for this start; the tokens a restored file holds are voided
// before anything reads them.
func openHubRegistry(ctx context.Context, dir string, now time.Time, log *slog.Logger) (*fleet.Registry, error) {
	before, restored, err := fleet.ApplyRestore(dir, now)
	if err != nil {
		return nil, fmt.Errorf("applying the staged restore: %w", err)
	}
	reg, err := fleet.OpenRegistry(ctx, dir)
	if err != nil {
		return nil, err
	}
	reg.Log = log
	if restored {
		// The backup brings back tokens revoked since it was taken
		// (staging voided them in the copy; a file put there by hand was
		// not staged).
		n, err := reg.VoidTokens(ctx, fleet.OriginLocal, "a backup was restored")
		if err != nil {
			reg.Close()
			return nil, err
		}
		log.Warn("fleet: a backup was restored; every API and join token is void, issue new ones. Members, roles and the clusters' settings and secrets are those of the backup now: review Members and Access",
			"replaced_file_kept_as", before, "voided", n)
	}
	return reg, nil
}

func hubMain(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return hubCommand(args[0], args[1:])
	}
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: ci-accelerator hub [flags]   (the hub role)\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), "\n"+strings.TrimSuffix(hubCommandsUsage, "Flags:\n"))
	}
	var lf logging.Flags
	lf.Register(fs)
	var gitlabURLs list
	showVersion := fs.Bool("version", false, "print the version and exit")
	listen := fs.String("listen", ":8080", "address of the pages, the API, the members' connections and the health check")
	data := fs.String("data", "/data", "directory of the hub's store (sessions) and its fleet.db (members, audit)")
	keyFile := fs.String("key-file", "", "the hub's private key (PEM, PKCS#8 or SEC1; Ed25519 or ECDSA P-256/P-384): members pin its public key")
	signInFile := fs.String("sign-in", "", "JSON file configuring sign-in through GitLab (owners as its admins); empty: no sign-in, the pages read-only for everyone who reaches them and the API only with -api-token-file")
	fs.Var(&gitlabURLs, "gitlab-url", "URL of a GitLab instance people sign in with (repeatable; also $GITLAB_URLS)")
	tokenFile := fs.String("api-token-file", "", "file with the bootstrap API token (role owner) of the management API, read at every use: valid while the file holds it; empty: none")
	caFile := fs.String("ca-cert-file", "", "CA certificate(s) of the hub's address (PEM), when not a public CA: written into the values the hub generates for its clusters")
	secretsKeyFile := fs.String("secrets-key-file", "", "file with the key (at least 32 characters) the hub seals the secrets it keeps for clusters with (runner tokens, S3 keys); empty: it keeps none")
	keepRemoved := fs.Duration("keep-removed", 30*24*time.Hour, "how long a removed member's record is kept (its name may be registered again at once)")
	devAssets := fs.String("dev-assets", "", "directory with templates/ and static/ read for every page instead of the built-in ones")
	fs.Parse(args)
	if *showVersion {
		fmt.Println("ci-accelerator " + versionString())
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q: a hub command goes first (ci-accelerator hub <command> [flags], see ci-accelerator hub help)", fs.Args())
	}
	if *keepRemoved < 0 {
		return errors.New("-keep-removed: give a duration of 0 or more")
	}
	log, err := roleLog(lf, "hub")
	if err != nil {
		return err
	}
	if *keyFile == "" {
		return errors.New("give -key-file: the hub's private key (ci-accelerator fleet-keygen makes one)")
	}
	kb, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	key, err := fleet.ParsePrivateKey(kb)
	if err != nil {
		return fmt.Errorf("-key-file: %w", err)
	}
	token := &bootstrapToken{path: *tokenFile, log: log}
	if *tokenFile != "" {
		// Its Secret may be gone already (an own token issued): no
		// bootstrap token then. One given but too short is a mistake.
		b, err := os.ReadFile(*tokenFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
			log.Info("fleet: no bootstrap API token now: its file is not there", "file", *tokenFile)
		case err != nil:
			return err
		default:
			if t := strings.TrimSpace(string(b)); t != "" && len(t) < minAPIToken {
				return errors.New("-api-token-file: the token must be at least 24 characters")
			}
		}
	}
	urls := append([]string(gitlabURLs), split(os.Getenv("GITLAB_URLS"))...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	started(log, "listen", *listen, "data", *data, "key", fleet.Fingerprint(key.Public()), "sign_in", *signInFile != "",
		"gitlab", hosts(urls), "api_token", *tokenFile != "", "keep_removed", keepRemoved.String(), "log_level", lf.Level)

	st, err := store.Open(ctx, *data, store.Options{Keep: 24 * time.Hour, KeepOutput: 1})
	if err != nil {
		return err
	}
	defer st.Close()
	reg, err := openHubRegistry(ctx, *data, time.Now(), log)
	if err != nil {
		return err
	}
	defer reg.Close()
	reg.KeepRemoved = *keepRemoved
	var sealer *fleet.Sealer
	if *secretsKeyFile != "" {
		b, err := os.ReadFile(*secretsKeyFile)
		if err != nil {
			return err
		}
		if sealer, err = fleet.NewSealer(b); err != nil {
			return fmt.Errorf("-secrets-key-file: %w", err)
		}
	} else {
		log.Info("fleet: no secrets key: the hub keeps no secrets for its clusters and delivers none")
	}
	var signIn *auth.Auth
	if *signInFile != "" {
		if len(urls) == 0 {
			return errors.New("-sign-in needs the GitLabs people sign in with: -gitlab-url (or $GITLAB_URLS)")
		}
		client := gitlab.New(urls)
		client.Log = log
		if signIn, err = loadSignIn(*signInFile, urls, st, client, log); err != nil {
			return err
		}
		for _, ch := range signIn.Instances() {
			log.Info("sign-in through GitLab", "gitlab", ch.Host, "redirect_uri", signIn.RedirectURL(ch.Instance))
		}
	}
	// The owners from the values are bindings of origin values: locked
	// here, gone when the values drop them.
	var owners []string
	if signIn != nil {
		for _, a := range signIn.Admins() {
			owners = append(owners, fleet.PersonSubject(a.Host, a.UserID))
		}
		// Operators and owners see every project, as a box's admins do.
		signIn.Elevate = func(host string, id uint64) bool {
			return reg.RoleOf(fleet.PersonSubject(host, id)).Allows(fleet.PermAllProjects)
		}
	}
	if err := reg.SyncOwners(ctx, owners); err != nil {
		return fmt.Errorf("the owners from the values: %w", err)
	}
	info := fleet.HubInfo{URL: strings.TrimRight(os.Getenv(web.PublicURLEnv), "/")}
	if pub, err := fleet.PublicPEM(key.Public()); err == nil {
		info.PublicKeys = string(pub)
	}
	if info.URL == "" {
		log.Warn("fleet: the hub has no console address ($" + web.PublicURLEnv + "): the values it generates for clusters do not install")
	}
	if *caFile != "" {
		b, err := os.ReadFile(*caFile)
		if err != nil {
			return fmt.Errorf("-ca-cert-file: %w", err)
		}
		info.CACert = string(b)
	}
	h, top, err := newHub(ctx, hubConfig{reg: reg, log: log, key: key, token: token, signIn: signIn, sealer: sealer,
		assets: *devAssets, version: versionString(), info: info})
	if err != nil {
		return err
	}

	go func() {
		for {
			if err := reg.Prune(ctx); err != nil {
				log.Warn("fleet: pruning", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(pruneEvery):
			}
		}
	}()
	go func() {
		// A command run in the hub's pod (hub grant) writes the file
		// directly: what it changed counts within this period. Failed
		// deliveries are tried again as they come due.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(refreshEvery):
			}
			if err := reg.Refresh(ctx); err != nil {
				log.Warn("fleet: reading the registry again", "err", err)
			}
			h.retryDeliveries(ctx, time.Now())
		}
	}()
	if signIn != nil {
		go prune(ctx, st, signIn, log)
	}

	// The requests' context ends a little after the stop begins: open
	// streams end then, short requests had their time.
	base, endRequests := context.WithCancel(context.Background())
	defer endRequests()
	srv := &http.Server{Handler: top, ReadHeaderTimeout: 10 * time.Second, ErrorLog: logging.ErrorLog(log),
		BaseContext: func(net.Listener) context.Context { return base }}
	srv.RegisterOnShutdown(func() {
		// The listener is closed now: members do not come back meanwhile.
		h.mu.Lock()
		for _, ml := range h.links {
			ml.end()
		}
		h.mu.Unlock()
		time.AfterFunc(stopWait/2, endRequests)
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	names := []string{}
	for _, m := range h.list(ctx) {
		if m.RemovedAt.IsZero() {
			names = append(names, m.Name)
		}
	}
	sort.Strings(names)
	log.Info("ready", "listen", ln.Addr().String(), "members", names)
	<-ctx.Done()
	log.Info("stopping")
	sctx, cancel := context.WithTimeout(context.Background(), stopWait)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("stopping: requests still open were cut", "err", err)
	}
	return nil
}

// deliveryLock is the lock of a member's deliveries.
func (h *hub) deliveryLock(name string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	mu := h.delivering[name]
	if mu == nil {
		mu = &sync.Mutex{}
		h.delivering[name] = mu
	}
	return mu
}

// deliver sends a member the Secrets the hub keeps for it, when some are
// not delivered yet (or always, with force: a member installed anew lost
// them); one delivery per member at a time. It says whether the member
// has what it should (nothing to deliver counts).
func (h *hub) deliver(ctx context.Context, name string, force bool) bool {
	mu := h.deliveryLock(name)
	mu.Lock()
	defer mu.Unlock()
	ok := h.deliverLocked(ctx, name, force)
	h.retried(name, ok, time.Now())
	return ok
}

// retried notes how a member's delivery went: a failed one is due again
// after a wait that doubles each time.
func (h *hub) retried(name string, ok bool, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		delete(h.retry, name)
		return
	}
	r := h.retry[name]
	if r == nil {
		r = &retryState{wait: retryFirst}
		h.retry[name] = r
	} else {
		r.wait = min(r.wait*2, retryMax)
	}
	r.next = now.Add(r.wait)
}

// retryDeliveries tries the deliveries due for the members connected now
// (each returns at once when nothing is undelivered); a member whose
// delivery is running is left to it.
func (h *hub) retryDeliveries(ctx context.Context, now time.Time) {
	if h.sealer == nil {
		return
	}
	h.mu.Lock()
	var due []string
	for n, ml := range h.links {
		if ml.pages == nil {
			continue
		}
		if r := h.retry[n]; r != nil && now.Before(r.next) {
			continue
		}
		due = append(due, n)
	}
	h.mu.Unlock()
	for _, n := range due {
		mu := h.deliveryLock(n)
		if !mu.TryLock() {
			continue
		}
		go func() {
			defer mu.Unlock()
			h.retried(n, h.deliverLocked(ctx, n, false), time.Now())
		}()
	}
}

func (h *hub) deliverLocked(ctx context.Context, name string, force bool) bool {
	if h.sealer == nil {
		return true
	}
	h.mu.Lock()
	ml := h.links[name]
	h.mu.Unlock()
	if ml == nil {
		return true
	}
	if !force {
		u, err := h.reg.Undelivered(ctx, name)
		if err != nil {
			h.log.Warn("fleet: reading what a member got", "name", name, "err", err)
			return false
		}
		if !u {
			return true
		}
	}
	o, err := h.reg.Outgoing(ctx, h.sealer, name)
	if err != nil {
		h.log.Error("fleet: opening a member's secrets", "name", name, "err", err)
		return false
	}
	if len(o.Stale) > 0 {
		h.log.Warn("fleet: kept secrets do not open with the secrets key (it was changed): enter them again", "name", name, "secrets", o.Stale)
	}
	if len(o.Secrets) == 0 {
		return true
	}
	body, _ := json.Marshal(map[string]any{"secrets": o.Secrets})
	dctx, cancel := context.WithTimeout(ctx, deliverWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(dctx, http.MethodPut, "http://"+name+SecretsPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ml.link.HTTP(deliverWait).Do(req)
	if err != nil {
		h.log.Warn("fleet: delivering a member's secrets", "name", name, "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		h.log.Warn("fleet: the member takes no secrets from the hub (an older version)", "name", name, "version", ml.link.Version)
		return false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDeliveryAnswer))
	var out struct {
		Written []string `json:"written"`
		Error   string   `json:"error"`
	}
	decoded := json.Unmarshal(raw, &out) == nil
	// Only what was sent counts as written: the member's answer is its
	// word, not the hub's record.
	var written []string
	for _, n := range out.Written {
		if _, sent := o.Secrets[n]; sent {
			written = append(written, n)
		}
	}
	if len(written) > 0 {
		if err := h.reg.DeliveredOutgoing(ctx, name, o, written); err != nil {
			h.log.Warn("fleet: recording a delivery", "name", name, "err", err)
		}
	}
	if resp.StatusCode != http.StatusOK || !decoded {
		answer := out.Error
		if !decoded {
			answer = strings.TrimSpace(string(raw[:min(len(raw), 4096)]))
		}
		h.log.Warn("fleet: the member did not write its secrets", "name", name, "status", resp.Status, "written", written, "answer", answer)
		return false
	}
	h.log.Info("fleet: delivered a member's secrets", "name", name, "secrets", written)
	return len(written) == len(o.Secrets)
}
