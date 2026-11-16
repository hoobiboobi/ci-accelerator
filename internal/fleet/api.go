package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the management API's version. "alpha": it may change
// between releases without the v1 contract's promise (the fleet's first
// stages are experimental).
const APIVersion = "1-alpha"

// Status is a member's link as the hub sees it now.
type Status struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitzero"`
	Remote    string    `json:"remote,omitempty"`
	Protocol  int       `json:"protocol,omitempty"`
}

// MemberView is a member with its link's state.
type MemberView struct {
	Member
	Status Status `json:"status"`
}

// API serves the hub's management API under /api/v1/fleet/.
type API struct {
	Registry *Registry
	// Who says who a request comes from; a zero Principal: nobody (no
	// token, no session).
	Who func(r *http.Request) Principal
	// Status is a member's link now.
	Status func(name string) Status
	// Removed is called after a member is removed (to close its link).
	Removed func(name string)
	Version string
	// Sealer keeps the clusters' secrets (nil: none kept); Deliver sends a
	// connected cluster the ones not delivered yet; Hub: what generated
	// values say about the hub.
	Sealer  *Sealer
	Deliver func(name string)
	Hub     HubInfo
	// SignInHosts are the hosts (host[:port], as sign-in writes them into
	// a subject) of the GitLabs people sign in with; a grant to another
	// host is refused. Empty: any host (no sign-in configured).
	SignInHosts []string
}

// apiRoute is one route of the API: its pattern, the permission it needs
// ("": anyone the hub knows) and its handler.
type apiRoute struct {
	pattern, perm string
	h             handler
}

// APIPrefix is where the management API is served.
const APIPrefix = "/api/v1/fleet/"

// routes are the API's routes, the one list Register serves (and the
// contract test checks against docs/api/openapi.yaml).
func (a *API) routes() []apiRoute {
	return []apiRoute{
		{"GET /api/v1/fleet/version", "", a.version},
		{"GET /api/v1/fleet/members", PermMembersRead, a.members},
		{"POST /api/v1/fleet/members", PermMembersWrite, a.add},
		{"GET /api/v1/fleet/members/{name}", PermMembersRead, a.member},
		{"DELETE /api/v1/fleet/members/{name}", PermMembersWrite, a.remove},
		{"GET /api/v1/fleet/members/{name}/join-tokens", PermMembersWrite, a.joins},
		{"POST /api/v1/fleet/members/{name}/join-tokens", PermMembersWrite, a.issueJoin},
		{"GET /api/v1/fleet/members/{name}/join-tokens/{id}", PermMembersWrite, a.joinTok},
		{"DELETE /api/v1/fleet/members/{name}/join-tokens/{id}", PermMembersWrite, a.revokeJoin},
		{"GET /api/v1/fleet/members/{name}/settings", PermMembersRead, a.settings},
		{"PUT /api/v1/fleet/members/{name}/settings", PermMembersWrite, a.putSettings},
		{"GET /api/v1/fleet/members/{name}/values", PermMembersRead, a.values},
		{"GET /api/v1/fleet/role-bindings", PermRolesRead, a.bindings},
		// A person's binding: gitlab:<host>/<id> as a path, no escaped
		// slash (proxies decode %2F).
		{"GET /api/v1/fleet/role-bindings/gitlab/{host}/{id}", PermRolesRead, a.binding},
		{"PUT /api/v1/fleet/role-bindings/gitlab/{host}/{id}", PermRolesWrite, a.grant},
		{"DELETE /api/v1/fleet/role-bindings/gitlab/{host}/{id}", PermRolesWrite, a.revoke},
		{"GET /api/v1/fleet/tokens", PermTokensWrite, a.tokens},
		{"POST /api/v1/fleet/tokens", PermTokensWrite, a.issue},
		{"GET /api/v1/fleet/tokens/{name}", PermTokensWrite, a.token},
		{"DELETE /api/v1/fleet/tokens/{name}", PermTokensWrite, a.revokeToken},
		{"GET /api/v1/fleet/audit", PermAuditRead, a.audit},
	}
}

// Register adds the API's routes to mux, and an answer in JSON for
// anything else under APIPrefix.
func (a *API) Register(mux *http.ServeMux) {
	for _, rt := range a.routes() {
		mux.HandleFunc(rt.pattern, a.guard(rt.perm, rt.h))
	}
	mux.HandleFunc(APIPrefix, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var allow []string
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			other := r.Clone(r.Context())
			other.Method = m
			if _, p := mux.Handler(other); p != APIPrefix && p != "" {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" is not one of "+strings.Join(allow, ", ")+" here")
			return
		}
		fail(w, http.StatusNotFound, "not_found", "no such path in the API")
	})
}

type handler func(w http.ResponseWriter, r *http.Request, actor string)

// guard lets a request through when its principal has perm ("": anyone
// the hub knows, viewers too).
func (a *API) guard(perm string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p := a.Who(r)
		if p.Subject == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ci-accelerator fleet hub"`)
			fail(w, http.StatusUnauthorized, "unauthorized", "give an API token of the hub (Authorization: Bearer ...) or sign in")
			return
		}
		if perm != "" && !p.Can(perm) {
			fail(w, http.StatusForbidden, "forbidden", "the role "+string(p.Role)+" may not do this (it needs "+perm+")")
			return
		}
		h(w, r, p.Subject)
	}
}

// maxBody is the largest request body the API reads.
const maxBody = 64 << 10

// decode reads a request's JSON body into v: one object, unknown fields
// and anything after it refused.
func decode(w http.ResponseWriter, r *http.Request, v any, what string) bool {
	return decodeBody(w, r, v, what, false)
}

// decodeBody is decode; optional: an empty body leaves v as it is.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, what string, optional bool) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a request body is at most %d bytes", maxBody))
		return false
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid", "reading the request: "+err.Error())
		return false
	}
	if optional && len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid", "a JSON object with "+what+": "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		fail(w, http.StatusBadRequest, "invalid", "a JSON object with "+what+", and nothing after it")
		return false
	}
	return true
}

// failErr answers an error of the registry.
func failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		fail(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, ErrExists):
		fail(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, ErrLocked):
		fail(w, http.StatusConflict, "locked", err.Error())
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", err.Error())
	default:
		fail(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// binding is one person's binding; a person without one is a viewer.
func (a *API) binding(w http.ResponseWriter, r *http.Request, _ string) {
	subject := pathSubject(r)
	if _, _, err := ParsePerson(subject); err != nil {
		failErr(w, err)
		return
	}
	b, err := a.Registry.binding(r.Context(), subject)
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", subject+" has no binding (a viewer)")
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, b)
}

func (a *API) bindings(w http.ResponseWriter, r *http.Request, _ string) {
	bs, err := a.Registry.Bindings(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"role_bindings": bs})
}

// pathSubject is the subject a binding's path names.
func pathSubject(r *http.Request) string {
	return "gitlab:" + r.PathValue("host") + "/" + r.PathValue("id")
}

// grant sets a person's role: PUT, the subject in the path, so that
// Terraform's create and update are one call.
func (a *API) grant(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &in, "role") {
		return
	}
	role, err := ParseRole(in.Role)
	if err != nil {
		failErr(w, err)
		return
	}
	host, id, err := ParsePerson(pathSubject(r))
	if err != nil {
		failErr(w, err)
		return
	}
	if !a.signsIn(host) {
		failErr(w, fmt.Errorf("%w: %s is none of the GitLabs people sign in with (%s)", ErrInvalid, host, strings.Join(a.SignInHosts, ", ")))
		return
	}
	subject := PersonSubject(host, id)
	if role == Viewer {
		// A viewer is no binding: nothing stored, nothing to answer.
		if err := a.Registry.Revoke(r.Context(), actor, subject); err != nil {
			failErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b, err := a.Registry.Grant(r.Context(), actor, OriginAPI, subject, role)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, b)
}

// signsIn tells whether people of host can sign in to the hub (true
// when the hub names no sign-in hosts).
func (a *API) signsIn(host string) bool {
	if len(a.SignInHosts) == 0 {
		return true
	}
	for _, h := range a.SignInHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func (a *API) revoke(w http.ResponseWriter, r *http.Request, actor string) {
	subject := pathSubject(r)
	if _, _, err := ParsePerson(subject); err != nil {
		failErr(w, err)
		return
	}
	if err := a.Registry.Revoke(r.Context(), actor, subject); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) tokens(w http.ResponseWriter, r *http.Request, _ string) {
	ts, err := a.Registry.Tokens(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"tokens": ts})
}

// TokenLifetimes are the lifetimes a token may be issued with; 0: never.
var TokenLifetimes = []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour, 0}

// DefaultTokenLifetime is a token's lifetime when none is chosen.
const DefaultTokenLifetime = 90 * 24 * time.Hour

func (a *API) issue(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
		// ExpiresInDays: 0 or absent: 90; -1: never.
		ExpiresInDays int `json:"expires_in_days"`
	}
	if !decode(w, r, &in, "name, role and expires_in_days") {
		return
	}
	role, err := ParseRole(in.Role)
	if err != nil {
		failErr(w, err)
		return
	}
	ttl := DefaultTokenLifetime
	switch {
	case in.ExpiresInDays == -1:
		ttl = 0
	case in.ExpiresInDays > 0 && in.ExpiresInDays <= 3650:
		ttl = time.Duration(in.ExpiresInDays) * 24 * time.Hour
	case in.ExpiresInDays != 0:
		failErr(w, fmt.Errorf("%w: expires_in_days is 1 to 3650, or -1 for never", ErrInvalid))
		return
	}
	t, secret, err := a.Registry.IssueToken(r.Context(), actor, in.Name, role, ttl)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/fleet/tokens/"+t.Name)
	reply(w, http.StatusCreated, map[string]any{"token": t, "secret": secret})
}

func (a *API) token(w http.ResponseWriter, r *http.Request, _ string) {
	t, err := a.Registry.token(r.Context(), r.PathValue("name"))
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "no token named "+strconv.Quote(r.PathValue("name")))
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, t)
}

// BootstrapToken is the name of the token in the hub's values (its
// Secret): not kept by the hub, so not revoked here.
const BootstrapToken = "bootstrap"

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request, actor string) {
	if r.PathValue("name") == BootstrapToken {
		failErr(w, fmt.Errorf("%w: the bootstrap token is the one in the hub's values (its Secret): change or remove it there", ErrLocked))
		return
	}
	if err := a.Registry.RevokeToken(r.Context(), actor, r.PathValue("name")); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) version(w http.ResponseWriter, r *http.Request, _ string) {
	// The link's versions the hub accepts (Protocols: none below 1).
	reply(w, http.StatusOK, map[string]any{"hub": a.Version, "api": APIVersion, "protocols": Protocols()})
}

func (a *API) view(m Member) MemberView {
	v := MemberView{Member: m}
	if m.RemovedAt.IsZero() && a.Status != nil {
		v.Status = a.Status(m.Name)
	}
	return v
}

func (a *API) members(w http.ResponseWriter, r *http.Request, _ string) {
	ms, err := a.Registry.List(r.Context())
	if err != nil {
		failErr(w, err)
		return
	}
	out := []MemberView{}
	for _, m := range ms {
		out = append(out, a.view(m))
	}
	reply(w, http.StatusOK, map[string]any{"members": out})
}

func (a *API) member(w http.ResponseWriter, r *http.Request, _ string) {
	m, err := a.Registry.Get(r.Context(), r.PathValue("name"))
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "no member named "+strconv.Quote(r.PathValue("name")))
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, a.view(m))
}

func (a *API) add(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &in, "name and public_key") {
		return
	}
	m, err := a.Registry.Add(r.Context(), actor, OriginAPI, in.Name, in.PublicKey)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/fleet/members/"+m.Name)
	reply(w, http.StatusCreated, a.view(m))
}

// remove takes a cluster out of the fleet: its member, its unused join
// tokens, its settings and kept secrets (as the console's removal does).
func (a *API) remove(w http.ResponseWriter, r *http.Request, actor string) {
	name := r.PathValue("name")
	removed, err := a.Registry.RemoveCluster(r.Context(), actor, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		failErr(w, err)
		return
	}
	if removed && a.Removed != nil {
		a.Removed(name)
	}
	// Removing what is not there is done already.
	w.WriteHeader(http.StatusNoContent)
}

// clusterSettings are a cluster's settings, or a 404 that says whether
// the name is a member without settings.
func (a *API) clusterSettings(w http.ResponseWriter, r *http.Request) (Settings, bool) {
	name := r.PathValue("name")
	s, err := a.Registry.Settings(r.Context(), name)
	if errors.Is(err, ErrNotFound) {
		if m, err := a.Registry.Get(r.Context(), name); err == nil && m.RemovedAt.IsZero() {
			fail(w, http.StatusNotFound, "not_found", "the member "+strconv.Quote(name)+" has no settings at the hub yet: PUT its settings first")
		} else {
			fail(w, http.StatusNotFound, "not_found", "no cluster named "+strconv.Quote(name)+" has settings at the hub")
		}
		return Settings{}, false
	}
	if err != nil {
		failErr(w, err)
		return Settings{}, false
	}
	return s, true
}

// settings are a cluster's settings: its values, and which secrets are
// kept (never their values), stale ones marked.
func (a *API) settings(w http.ResponseWriter, r *http.Request, _ string) {
	s, ok := a.clusterSettings(w, r)
	if !ok {
		return
	}
	if err := a.Registry.MarkStale(r.Context(), a.Sealer, &s); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, s)
}

// putSettings replaces a cluster's settings; a secret not given stays as
// kept. Answers the settings as kept now.
func (a *API) putSettings(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Values map[string]string `json:"values"`
		// Forget: kept secrets to forget (optional ones).
		Forget []string `json:"forget"`
	}
	if !decode(w, r, &in, "values (and forget)") {
		return
	}
	name := r.PathValue("name")
	s, err := a.Registry.saveSettings(r.Context(), a.Sealer, actor, name, in.Values, in.Forget)
	var se *SettingsError
	if errors.As(err, &se) {
		reply(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid", "message": err.Error(), "fields": se.Fields}})
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	if a.Deliver != nil {
		a.Deliver(name)
	}
	if err := a.Registry.MarkStale(r.Context(), a.Sealer, &s); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, s)
}

// values is the box's values file the hub generates from a cluster's
// settings (YAML).
func (a *API) values(w http.ResponseWriter, r *http.Request, _ string) {
	s, ok := a.clusterSettings(w, r)
	if !ok {
		return
	}
	in := Generate(s, a.Hub, a.Version, "", false)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+in.File+`"`)
	w.Write([]byte(in.Values))
}

func (a *API) joins(w http.ResponseWriter, r *http.Request, _ string) {
	js, err := a.Registry.JoinTokens(r.Context(), r.PathValue("name"))
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"join_tokens": js})
}

// issueJoin makes a join token for a cluster about to be installed: the
// name must not be registered.
func (a *API) issueJoin(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		// ExpiresInMinutes: 0 or absent: 60; at most 7 days.
		ExpiresInMinutes int `json:"expires_in_minutes"`
	}
	if !decodeBody(w, r, &in, "expires_in_minutes", true) {
		return
	}
	if in.ExpiresInMinutes < 0 || in.ExpiresInMinutes > int(MaxJoinLifetime/time.Minute) {
		failErr(w, fmt.Errorf("%w: expires_in_minutes is 1 to %d", ErrInvalid, int(MaxJoinLifetime/time.Minute)))
		return
	}
	j, secret, err := a.Registry.IssueJoin(r.Context(), actor, r.PathValue("name"), time.Duration(in.ExpiresInMinutes)*time.Minute)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/fleet/members/"+j.Member+"/join-tokens/"+strconv.FormatInt(j.ID, 10))
	reply(w, http.StatusCreated, map[string]any{"join_token": j, "secret": secret})
}

// joinTok is one join token of a member name (never its secret).
func (a *API) joinTok(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		failErr(w, fmt.Errorf("%w: a join token's id is a number", ErrInvalid))
		return
	}
	j, err := a.Registry.joinToken(r.Context(), id)
	if errors.Is(err, ErrNotFound) || (err == nil && j.Member != r.PathValue("name")) {
		fail(w, http.StatusNotFound, "not_found", "no join token #"+r.PathValue("id")+" for "+strconv.Quote(r.PathValue("name")))
		return
	}
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, j)
}

func (a *API) revokeJoin(w http.ResponseWriter, r *http.Request, actor string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		failErr(w, fmt.Errorf("%w: a join token's id is a number", ErrInvalid))
		return
	}
	if err := a.Registry.RevokeJoin(r.Context(), actor, r.PathValue("name"), id); err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) audit(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	limit, before := 100, int64(0)
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 1000 {
			failErr(w, fmt.Errorf("%w: limit is a number from 1 to 1000", ErrInvalid))
			return
		}
		limit = n
	}
	if q.Has("before") {
		n, err := strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil || n < 1 {
			failErr(w, fmt.Errorf("%w: before is a record's id", ErrInvalid))
			return
		}
		before = n
	}
	es, err := a.Registry.AuditQuery(r.Context(), AuditQuery{Limit: limit, Before: before, Actor: q.Get("actor"), Target: q.Get("target")})
	if err != nil {
		failErr(w, err)
		return
	}
	if es == nil {
		es = []Event{}
	}
	reply(w, http.StatusOK, map[string]any{"events": es})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, kind, msg string) {
	reply(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
}
