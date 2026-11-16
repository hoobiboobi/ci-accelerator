package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// A hub's administration pages: who has which role (Access), its API
// tokens and the audit trail. Without sign-in they read for everyone and
// change nothing: changes then come only through the API with a token.

// Hub is what the hub's administration pages work with.
type Hub struct {
	Registry *fleet.Registry
	// Who is the principal of a request: the person signed in, with their
	// role at the hub.
	Who func(r *http.Request) fleet.Principal
	// Bootstrap: the values give a bootstrap API token (listed, not
	// managed here).
	Bootstrap bool
	// GitLabs people sign in with: host names, for "Give a role".
	GitLabs []string
	// Sealer keeps the clusters' secrets; nil: the hub keeps none (the
	// form's secret fields are off).
	Sealer *fleet.Sealer
	// Deliver sends a cluster the secrets not delivered yet (when it is
	// connected).
	Deliver func(name string)
	// Removed is called after a cluster is removed (to close its link).
	Removed func(name string)
	// NoOwners says nobody may manage the hub yet: the sign-in page says
	// how to set it up.
	NoOwners func() bool
	// Info: what the generated values say about the hub (its address and
	// keys); Version: the chart version the commands install.
	Info    fleet.HubInfo
	Version string
	// Events: what the hub saw of each cluster's link, for its page (nil:
	// none kept).
	Events *fleet.LinkEvents
}

// HubAccess is what the person on a page may do at the hub.
type HubAccess struct {
	// Members, Audit, Roles: may read those pages.
	Members, Audit, Roles bool
	// RolesWrite, Tokens: may change roles, see and change API tokens.
	RolesWrite, Tokens bool
	// SignIn: people sign in (without it nothing changes from the pages).
	SignIn bool
}

// hubAccess is what the person on r may do; without a hub, nothing.
func (s *Server) hubAccess(r *http.Request) HubAccess {
	h := s.opt.Hub
	if h == nil {
		return HubAccess{}
	}
	if s.opt.Auth == nil {
		// Read for everyone; change through the API only.
		return HubAccess{Members: true, Audit: true, Roles: true}
	}
	p := h.Who(r)
	return HubAccess{SignIn: true, Members: p.Can(fleet.PermMembersRead), Audit: p.Can(fleet.PermAuditRead),
		Roles: p.Can(fleet.PermRolesRead), RolesWrite: p.Can(fleet.PermRolesWrite), Tokens: p.Can(fleet.PermTokensWrite)}
}

// AccessPage is the hub's Access page: people with a role, or API tokens.
type AccessPage struct {
	Shell
	Tab    string // people, tokens
	Access HubAccess
	People []PersonRow
	Tokens []TokenRow
	// TokenCount for the tab while People shows.
	TokenCount int
	Bootstrap  bool
	GitLabs    []string
	Roles      []fleet.Role
	Lifetimes  []Option
	// Secret: a token just issued, shown this once.
	Secret, SecretName string
	// Message after a change; Bad: it failed.
	Message string
	Bad     bool
}

// PersonRow is a person with a role.
type PersonRow struct {
	fleet.Binding
	Name, Who string // the label and the subject without "gitlab:"
	Added     Ago
	By        string
	Locked    bool
	// Self: the person looking (their own role is changed by another owner).
	Self bool
}

// TokenRow is an API token.
type TokenRow struct {
	fleet.Token
	Expires, Used Ago
	Never         bool // expires never
	State         string
	By            string
}

func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	if s.hubAccess(r).Roles && s.accessConfirm(w, r) {
		return
	}
	s.accessPage(w, r, "", "", "", false)
}

func (s *Server) accessPage(w http.ResponseWriter, r *http.Request, tab, secretName, secret string, done bool) {
	acc := s.hubAccess(r)
	if !acc.Roles {
		s.forbidden(w, r, "Access shows who may manage this fleet: operators and owners see it.")
		return
	}
	if tab == "" {
		tab = "people"
		if r.URL.Path == "/access/tokens" {
			tab = "tokens"
		}
	}
	if tab == "tokens" && !acc.Tokens {
		s.forbidden(w, r, "API tokens are for owners.")
		return
	}
	sh, err := s.shell(r, "access", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Access"
	reg := s.opt.Hub.Registry
	v := AccessPage{Shell: sh, Tab: tab, Access: acc, Bootstrap: s.opt.Hub.Bootstrap, GitLabs: s.opt.Hub.GitLabs,
		Roles: []fleet.Role{fleet.Operator, fleet.Owner}, Secret: secret, SecretName: secretName}
	if !done {
		v.Message, v.Bad = s.flash(w, r)
	}
	now := s.now()
	bs, err := reg.Bindings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	me := ""
	if acc.SignIn {
		me = s.opt.Hub.Who(r).Subject
	}
	for _, b := range bs {
		row := PersonRow{Binding: b, Who: strings.TrimPrefix(b.Subject, "gitlab:"), Added: agoAt(b.AddedAt, now),
			By: person(reg, b.AddedBy), Locked: b.Origin == fleet.OriginValues, Self: me != "" && b.Subject == me}
		row.Name = b.Label
		v.People = append(v.People, row)
	}
	if acc.Tokens {
		ts, err := reg.Tokens(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if v.Bootstrap {
			v.TokenCount++
		}
		for _, t := range ts {
			row := TokenRow{Token: t, Used: agoAt(t.UsedAt, now), By: person(reg, t.CreatedBy), Never: t.ExpiresAt.IsZero()}
			if !t.ExpiresAt.IsZero() {
				row.Expires = Ago{Text: t.ExpiresAt.In(zone(r)).Format("Jan 2, 2006"), UTC: t.ExpiresAt.Format(time.RFC3339)}
			}
			switch {
			case !t.RevokedAt.IsZero():
				row.State = "revoked"
			case !t.Active(now):
				row.State = "expired"
			default:
				// The tab counts the tokens that open the hub.
				v.TokenCount++
			}
			v.Tokens = append(v.Tokens, row)
		}
		for _, d := range fleet.TokenLifetimes {
			o := Option{Value: strconv.Itoa(int(d / (24 * time.Hour))), Label: unitN(int(d/(24*time.Hour)), "day")}
			if d == 0 {
				o = Option{Value: "-1", Label: "never"}
			}
			o.On = d == fleet.DefaultTokenLifetime
			v.Lifetimes = append(v.Lifetimes, o)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "access", http.StatusOK, v)
}

// flashCookie carries the message of a change to the page it leads to,
// once. Not the address: a link could put any text there, shown as the
// hub's own words.
const flashCookie = "hub-msg"

// back answers a change of a hub page: to the page, which says msg.
func (s *Server) back(w http.ResponseWriter, r *http.Request, to, msg string, bad bool) {
	flag := "0"
	if bad {
		flag = "1"
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(flag + msg), Path: "/", MaxAge: 60,
		Secure: s.opt.Auth != nil || r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	s.redirect(w, r, safeBack(to), http.StatusSeeOther)
}

// safeBack is a page address back() may send to: its path segments
// escaped (a name taken from the request's address may hold anything), a
// query kept as it is; anything that is not a path of the site is "/".
func safeBack(to string) string {
	p, q, _ := strings.Cut(to, "?")
	if !sitePath(p) {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if u, err := url.PathUnescape(seg); err == nil {
			seg = u
		}
		if seg == "." || seg == ".." {
			return "/"
		}
		segs[i] = url.PathEscape(seg)
	}
	out := strings.Join(segs, "/")
	if q != "" {
		if strings.IndexFunc(q, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return out
		}
		out += "?" + q
	}
	return out
}

// flash is the message back left for this page, and forgets it.
func (s *Server) flash(w http.ResponseWriter, r *http.Request) (msg string, bad bool) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1,
		Secure: s.opt.Auth != nil || r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	v, err := url.QueryUnescape(c.Value)
	if err != nil || len(v) < 2 {
		return "", false
	}
	return v[1:], v[0] == '1'
}

// person names a subject as the pages show it: the label a person chose
// in GitLab, then the subject itself, which no label can fake.
func person(reg *fleet.Registry, subject string) string {
	id := strings.TrimPrefix(subject, "gitlab:")
	if l := reg.Label(subject); l != subject && l != "" {
		return l + " (" + id + ")"
	}
	return id
}

// errText is a registry error as the pages say it.
func errText(err error) string {
	msg := err.Error()
	for _, e := range []error{fleet.ErrInvalid, fleet.ErrExists, fleet.ErrLocked} {
		if errors.Is(err, e) {
			msg = strings.TrimPrefix(msg, e.Error()+": ")
		}
	}
	return msg
}

func (s *Server) grantRole(w http.ResponseWriter, r *http.Request) {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermRolesWrite) {
		s.forbidden(w, r, "Roles are given by owners.")
		return
	}
	subject := r.PostFormValue("subject")
	if subject == "" {
		host := strings.ToLower(strings.TrimSpace(r.PostFormValue("gitlab")))
		id := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("user")), "id:")
		subject = "gitlab:" + host + "/" + id
	}
	if subject == p.Subject {
		s.back(w, r, "/access", "Your own role is changed by another owner.", true)
		return
	}
	// A host nobody signs in with would give a role nobody holds.
	if host, _, perr := fleet.ParsePerson(subject); perr == nil && len(s.opt.Hub.GitLabs) > 0 && !signsInWith(s.opt.Hub.GitLabs, host) {
		s.back(w, r, "/access", host+" is none of the GitLabs people sign in with", true)
		return
	}
	role, err := fleet.ParseRole(r.PostFormValue("role"))
	if err == nil && !confirmed(r) {
		// A role changed (not one given) is asked once more.
		b, found, berr := s.binding(r, subject)
		if berr != nil {
			s.fail(w, berr)
			return
		}
		if found && b.Role != role {
			s.confirmRole(w, r, subject, string(role))
			return
		}
	}
	if err == nil {
		_, err = s.opt.Hub.Registry.Grant(r.Context(), p.Subject, fleet.OriginUI, subject, role)
	}
	if err != nil {
		s.back(w, r, "/access", errText(err), true)
		return
	}
	s.back(w, r, "/access", strings.TrimPrefix(subject, "gitlab:")+" is "+string(role)+" now.", false)
}

// signsInWith says whether host is one of the sign-in GitLabs' hosts.
func signsInWith(gitlabs []string, host string) bool {
	for _, g := range gitlabs {
		if strings.EqualFold(g, host) {
			return true
		}
	}
	return false
}

func (s *Server) revokeRole(w http.ResponseWriter, r *http.Request) {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermRolesWrite) {
		s.forbidden(w, r, "Roles are taken away by owners.")
		return
	}
	subject := r.PostFormValue("subject")
	if subject == p.Subject {
		s.back(w, r, "/access", "Your own role is changed by another owner.", true)
		return
	}
	if !confirmed(r) {
		s.confirmRevokeRole(w, r, subject)
		return
	}
	if err := s.opt.Hub.Registry.Revoke(r.Context(), p.Subject, subject); err != nil {
		s.back(w, r, "/access", errText(err), true)
		return
	}
	s.back(w, r, "/access", person(s.opt.Hub.Registry, subject)+" has no role now (a viewer).", false)
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermTokensWrite) {
		s.forbidden(w, r, "API tokens are issued by owners.")
		return
	}
	role, err := fleet.ParseRole(r.PostFormValue("role"))
	ttl := fleet.DefaultTokenLifetime
	if d := r.PostFormValue("days"); d != "" {
		// As the API takes them: 1 to 3650 days, or never (-1).
		days, perr := strconv.Atoi(d)
		switch {
		case perr == nil && days == -1:
			ttl = 0
		case perr == nil && days > 0 && days <= 3650:
			ttl = time.Duration(days) * 24 * time.Hour
		default:
			s.back(w, r, "/access/tokens", "A token lives 1 to 3650 days, or never expires.", true)
			return
		}
	}
	var t fleet.Token
	var secret string
	if err == nil {
		t, secret, err = s.opt.Hub.Registry.IssueToken(r.Context(), p.Subject, strings.TrimSpace(r.PostFormValue("name")), role, ttl)
	}
	if err != nil {
		s.back(w, r, "/access/tokens", errText(err), true)
		return
	}
	// The secret is on this answer only: never in an address, never kept.
	s.accessPage(w, r, "tokens", t.Name, secret, true)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermTokensWrite) {
		s.forbidden(w, r, "API tokens are revoked by owners.")
		return
	}
	name := r.PostFormValue("name")
	if !confirmed(r) {
		s.confirmRevokeToken(w, r, name)
		return
	}
	if err := s.opt.Hub.Registry.RevokeToken(r.Context(), p.Subject, name); err != nil {
		s.back(w, r, "/access/tokens", errText(err), true)
		return
	}
	s.back(w, r, "/access/tokens", "The token "+name+" is revoked: it opens nothing from now on.", false)
}

// AuditPage is the hub's audit trail.
type AuditPage struct {
	Shell
	Rows []AuditRow
	// Who, Target: the filters; Actors, Targets: the choices.
	Who, Target     string
	Actors, Targets []Option
	// Older: the address of the next page; "" at the end.
	Older string
}

// AuditRow is one audit record.
type AuditRow struct {
	fleet.Event
	When Ago
	// At: the record's time in the reader's zone.
	At Stamp
	// What: the action in words (Action is its id).
	What   string
	Actor  string // as the pages name it
	Target string
}

// auditWords are the audit's actions as the page says them.
var auditWords = map[string]string{
	"member.add":            "added a cluster by its key",
	"member.join":           "joined with a join token",
	"member.remove":         "removed a cluster",
	"member.store-replaced": "came with another store",
	"join-token.issue":      "issued a join token",
	"join-token.revoke":     "revoked a join token",
	"settings.create":       "saved a cluster's settings",
	"settings.change":       "changed a cluster's settings",
	"settings.delete":       "forgot a cluster's settings and secrets",
	"secrets.deliver":       "received its secrets",
	"role.grant":            "gave a role",
	"role.revoke":           "took a role away",
	"token.issue":           "issued an API token",
	"token.revoke":          "revoked an API token",
	"token.void":            "voided every token",
	"hub.backup":            "backed up the hub",
}

// auditFilterSpan is how far back the filters' choices look.
const auditFilterSpan = 1000

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	if !s.hubAccess(r).Audit {
		s.forbidden(w, r, "The audit trail is for operators and owners.")
		return
	}
	sh, err := s.shell(r, "audit", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Audit"
	reg := s.opt.Hub.Registry
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	v := AuditPage{Shell: sh, Who: q.Get("who"), Target: q.Get("target")}
	const page = 100
	es, err := reg.AuditQuery(r.Context(), fleet.AuditQuery{Limit: page + 1, Before: before, Actor: v.Who, Target: v.Target})
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(es) > page {
		es = es[:page]
		older := url.Values{"before": {strconv.FormatInt(es[len(es)-1].ID, 10)}}
		if v.Who != "" {
			older.Set("who", v.Who)
		}
		if v.Target != "" {
			older.Set("target", v.Target)
		}
		v.Older = "/audit?" + older.Encode()
	}
	now := s.now()
	for _, e := range es {
		what := auditWords[e.Action]
		if what == "" {
			what = e.Action
		}
		v.Rows = append(v.Rows, AuditRow{Event: e, When: agoAt(e.At, now), At: stampOf(e.At, zone(r), "Jan 2 15:04"), What: what,
			Actor: person(reg, e.Actor), Target: person(reg, e.Target)})
	}
	// The filters' choices: who and what appear in the recent records.
	recent, err := reg.AuditQuery(r.Context(), fleet.AuditQuery{Limit: auditFilterSpan})
	if err != nil {
		s.fail(w, err)
		return
	}
	seenA, seenT := map[string]bool{}, map[string]bool{}
	for _, e := range recent {
		if !seenA[e.Actor] {
			seenA[e.Actor] = true
			v.Actors = append(v.Actors, Option{Value: e.Actor, Label: person(reg, e.Actor), On: e.Actor == v.Who})
		}
		if !seenT[e.Target] {
			seenT[e.Target] = true
			v.Targets = append(v.Targets, Option{Value: e.Target, Label: person(reg, e.Target), On: e.Target == v.Target})
		}
	}
	// A filter from older records stays chosen.
	if v.Who != "" && !seenA[v.Who] {
		v.Actors = append(v.Actors, Option{Value: v.Who, Label: person(reg, v.Who), On: true})
	}
	if v.Target != "" && !seenT[v.Target] {
		v.Targets = append(v.Targets, Option{Value: v.Target, Label: person(reg, v.Target), On: true})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "audit", http.StatusOK, v)
}

// forbidden is the page for a part of the hub the person's role does not
// open.
func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, why string) {
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Not for your role"
	how := " An owner can give you a role."
	if s.opt.Auth == nil {
		sh.Title = "Not on these pages"
		how = " Sign-in is off on this hub, so nobody holds a role here: changes go through the hub's API (/api/v1/fleet/) with an API token."
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "notfound", http.StatusForbidden, NotFound{Shell: sh, What: "page", Hint: why + how,
		Links: []Option{{Value: "/~/", Label: "The fleet"}}})
}
