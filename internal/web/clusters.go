package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// "Add a cluster" and a cluster's page at the hub: its settings from the
// curated list (fleet.Params), the values and commands the hub generates,
// a new join token, removal. Operators and owners only.

// joining are the settings of how a cluster joins: in the form that adds
// it, not in the one that changes it.
var joining = map[string]bool{"joins": true, "token_hours": true, "public_key": true}

// ClusterForm is the "Add a cluster" form, or a cluster's "Change
// settings".
type ClusterForm struct {
	Shell
	// Name: the cluster's; Change: the form changes a cluster's settings.
	Name     string
	Change   bool
	Sections []FormSection
	// NameError: why the name was refused.
	NameError string
	// Message: why the form came back.
	Message string
	// NoSecrets: the hub keeps no secrets (no secrets key).
	NoSecrets bool
	// Refused: how many settings were refused.
	Refused int
}

// FormSection is one section of the form.
type FormSection struct {
	Title  string
	Fields []FormField
}

// FormField is one setting in the form.
type FormField struct {
	fleet.Param
	Value   string
	Options []Option
	// Kept: a secret the hub keeps (the field changes it).
	Kept  bool
	Error string
	// Focus: the first refused field.
	Focus bool
}

func (s *Server) canManage(r *http.Request) bool {
	return s.opt.Auth != nil && s.opt.Hub.Who(r).Can(fleet.PermMembersWrite)
}

func (s *Server) clusterForm(w http.ResponseWriter, r *http.Request, name string, change bool, values map[string]string,
	kept map[string]bool, bad []fleet.FieldError, msg string, status int) {
	sh, err := s.shell(r, "members", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Add a cluster"
	if change {
		sh.Title = "Change " + name
	}
	v := ClusterForm{Shell: sh, Name: name, Change: change, Message: msg, NoSecrets: s.opt.Hub.Sealer == nil}
	errs := map[string]string{}
	for _, b := range bad {
		if b.Key == "name" {
			v.NameError = b.Msg
		}
		errs[b.Key] = b.Msg
	}
	v.Refused = len(errs)
	focus := v.NameError == ""
	for _, p := range fleet.Params {
		if change && joining[p.Key] {
			// How a cluster joined is not changed afterwards.
			continue
		}
		if len(v.Sections) == 0 || v.Sections[len(v.Sections)-1].Title != p.Section {
			v.Sections = append(v.Sections, FormSection{Title: p.Section})
		}
		f := FormField{Param: p, Value: values[p.Key], Kept: kept[p.Key], Error: errs[p.Key]}
		if f.Error != "" && focus {
			f.Focus, focus = true, false
		}
		if f.Value == "" && p.Kind != fleet.KindSecret && values == nil {
			f.Value = p.Default
		}
		for _, c := range p.Choices {
			cur := f.Value
			if cur == "" {
				cur = p.Default
			}
			f.Options = append(f.Options, Option{Value: c[0], Label: c[1], On: c[0] == cur})
		}
		v.Sections[len(v.Sections)-1].Fields = append(v.Sections[len(v.Sections)-1].Fields, f)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "clusterform", status, v)
}

func (s *Server) newCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Clusters are added by operators and owners.")
		return
	}
	s.clusterForm(w, r, "", false, nil, nil, nil, "", http.StatusOK)
}

// formValues are the settings posted by the form.
func formValues(r *http.Request) map[string]string {
	r.ParseForm()
	out := map[string]string{}
	for _, p := range fleet.Params {
		if v := strings.TrimSpace(r.PostForm.Get(p.Key)); v != "" {
			out[p.Key] = v
		}
	}
	return out
}

// InstallPage hands over a cluster's values and commands.
type InstallPage struct {
	Shell
	Name    string
	Install fleet.Install
	// Expires: when the join token in the commands stops working.
	Expires Stamp
	// Connected: the cluster is connected now (an upgrade's page).
	Connected bool
	// Waiting: the cluster has not joined yet and has no join token in
	// these commands.
	Waiting bool
}

// joinHours is how long a cluster's join tokens live, from its settings.
func joinHours(values map[string]string) time.Duration {
	hours, _ := strconv.Atoi(values["token_hours"])
	if hours <= 0 {
		hours = 1
	}
	return time.Duration(hours) * time.Hour
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Clusters are added by operators and owners.")
		return
	}
	p := s.opt.Hub.Who(r)
	reg := s.opt.Hub.Registry
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	values := formValues(r)
	again := func(bad []fleet.FieldError, msg string) {
		s.clusterForm(w, r, name, false, values, nil, bad, msg, http.StatusUnprocessableEntity)
	}
	if !fleet.NamePattern.MatchString(name) {
		again([]fleet.FieldError{{Key: "name", Msg: "lower-case letters, digits and dashes, at most 40, starting and ending with a letter or digit"}}, "")
		return
	}
	if name == "new" {
		// GET /clusters/new is this form: such a cluster's page would not open.
		again([]fleet.FieldError{{Key: "name", Msg: `the name "new" is the address of the Add a cluster form; choose another`}}, "")
		return
	}
	if m, err := reg.Get(ctx, name); err == nil && m.RemovedAt.IsZero() {
		again([]fleet.FieldError{{Key: "name", Msg: "a cluster of this name is in the fleet"}}, "")
		return
	}
	if _, err := reg.Settings(ctx, name); err == nil {
		again([]fleet.FieldError{{Key: "name", Msg: "a cluster of this name is waiting to join: open it on Members"}}, "")
		return
	}
	var kept map[string]bool
	if s.opt.Hub.Sealer == nil {
		// No secrets key: the form asks for no secrets, they are made in
		// the cluster by hand.
		kept = fleet.SecretsMadeByHand()
	}
	if bad := fleet.CheckSettings(values, kept); len(bad) > 0 {
		again(bad, "")
		return
	}
	set, err := reg.SaveSettings(ctx, s.opt.Hub.Sealer, p.Subject, name, values)
	var se *fleet.SettingsError
	if errors.As(err, &se) {
		again(se.Fields, "")
		return
	}
	if err != nil {
		again(nil, errText(err))
		return
	}
	v := InstallPage{Name: name}
	token := ""
	if values["joins"] == "key" {
		if _, err := reg.Add(ctx, p.Subject, fleet.OriginUI, name, values["public_key"]); err != nil {
			reg.DeleteSettings(ctx, p.Subject, name)
			again([]fleet.FieldError{{Key: "public_key", Msg: errText(err)}}, "")
			return
		}
	} else {
		j, secret, err := reg.IssueJoin(ctx, p.Subject, name, joinHours(values))
		if err != nil {
			reg.DeleteSettings(ctx, p.Subject, name)
			again(nil, errText(err))
			return
		}
		token = secret
		v.Expires = stampOf(j.ExpiresAt, zone(r), "Jan 2 15:04")
	}
	v.Install = fleet.Generate(set, s.opt.Hub.Info, s.opt.Hub.Version, token, false)
	s.installPage(w, r, v)
}

func (s *Server) installPage(w http.ResponseWriter, r *http.Request, v InstallPage) {
	sh, err := s.shell(r, "members", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Install " + v.Name
	v.Shell = sh
	// The join token is on this answer only.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "install", http.StatusOK, v)
}

// ClusterPage is one cluster at the hub: its state and settings.
type ClusterPage struct {
	Shell
	Member *FleetMember
	// Settings: nil for a cluster registered by its key through the API.
	Settings *fleet.Settings
	Rows     []SettingRow
	Secrets  []SecretRow
	Joins    []JoinRow
	Waiting  bool // settings, no registration yet
	Manage   bool
	Message  string
	Bad      bool
	// Changed: when the settings were last changed.
	Changed Stamp
	// StoreReplaced: when the cluster last came with another store (its
	// builds from before then are gone); Audit: the person reads the audit.
	StoreReplaced Stamp
	Audit         bool
	// Events: the hub keeps link events; LinkEvents: this cluster's, the
	// newest first.
	Events     bool
	LinkEvents []LinkEventRow
	// JoinVoids: a new join token would void one still unused (it is
	// asked once more).
	JoinVoids bool
	// RemoveText: what removing it does.
	RemoveText string
}

// LinkEventRow is a link event as the cluster's page shows it.
type LinkEventRow struct {
	At   Stamp
	Text string
}

// SettingRow is a setting as the cluster's page shows it.
type SettingRow struct{ Section, Label, Value string }

// SecretRow is a secret the hub keeps for the cluster.
type SecretRow struct {
	Label     string
	Updated   Ago
	By        string
	Delivered Ago
	// Stale: it does not open with the hub's secrets key (the key changed).
	Stale bool
}

// JoinRow is a join token of the cluster.
type JoinRow struct {
	ID      int64
	Created Ago
	By      string
	State   string
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	if !s.hubAccess(r).Members {
		s.forbidden(w, r, "A cluster's settings are for operators and owners.")
		return
	}
	name := r.PathValue("name")
	reg := s.opt.Hub.Registry
	ctx := r.Context()
	v := ClusterPage{Manage: s.canManage(r)}
	if v.Manage && r.URL.Query().Get("confirm") == "join" {
		// "New join token" when it would void one still unused.
		if _, err := reg.Settings(ctx, name); err == nil && s.confirmJoin(w, r, name) {
			return
		}
	}
	for _, m := range s.opt.Fleet.Members() {
		if m.Name == name && m.State != "removed" {
			v.Member = &m
		}
	}
	if set, err := reg.Settings(ctx, name); err == nil {
		if err := reg.MarkStale(ctx, s.opt.Hub.Sealer, &set); err != nil {
			s.fail(w, err)
			return
		}
		v.Settings = &set
	} else if !errors.Is(err, fleet.ErrNotFound) {
		s.fail(w, err)
		return
	}
	js, err := reg.JoinTokens(ctx, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.now()
	if v.Member == nil && v.Settings == nil && !anyActive(js, now) {
		s.notFound(w, r, "cluster")
		return
	}
	v.Message, v.Bad = s.flash(w, r)
	v.Waiting = v.Member == nil
	if v.Settings != nil {
		v.Changed = stampOf(v.Settings.UpdatedAt, zone(r), "Jan 2 15:04")
		for _, p := range fleet.Params {
			if p.Kind == fleet.KindSecret || !p.Applies(v.Settings.Values) {
				continue
			}
			val := v.Settings.Values[p.Key]
			if val == "" {
				val = p.Default
			}
			for _, c := range p.Choices {
				if c[0] == val {
					val = c[1]
				}
			}
			if p.Key == "public_key" {
				continue
			}
			if val != "" {
				v.Rows = append(v.Rows, SettingRow{Section: p.Section, Label: p.Label, Value: val})
			}
		}
		for _, k := range v.Settings.Secrets {
			p, _ := fleet.ParamByKey(k.Param)
			v.Secrets = append(v.Secrets, SecretRow{Label: p.Label, Updated: agoAt(k.UpdatedAt, now), By: person(reg, k.UpdatedBy), Delivered: agoAt(k.DeliveredAt, now), Stale: k.Stale})
		}
	}
	for _, j := range js {
		row := JoinRow{ID: j.ID, Created: agoAt(j.CreatedAt, now), By: person(reg, j.CreatedBy)}
		switch {
		case !j.UsedAt.IsZero():
			row.State = "used " + agoAt(j.UsedAt, now).Text
		case !j.RevokedAt.IsZero():
			row.State = "revoked"
		case !j.Active(now):
			row.State = "expired"
		default:
			row.State = "valid until " + stampOf(j.ExpiresAt, zone(r), "Jan 2 15:04").Text
		}
		v.Joins = append(v.Joins, row)
	}
	if v.Member != nil && !v.Member.StoreReplaced.IsZero() {
		v.StoreReplaced = stampOf(v.Member.StoreReplaced, zone(r), "Jan 2, 2006 15:04")
	}
	v.Audit = s.hubAccess(r).Audit
	if ev := s.opt.Hub.Events; ev != nil {
		v.Events = true
		for _, e := range ev.Last(name) {
			v.LinkEvents = append(v.LinkEvents, LinkEventRow{At: stampOf(e.At, zone(r), "Jan 2 15:04:05"), Text: e.Text})
		}
	}
	v.JoinVoids = anyActive(js, now)
	v.RemoveText = removeText(v.Member, v.Settings, js, now)
	sh, err := s.shell(r, "members", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = name
	v.Shell = sh
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "cluster", http.StatusOK, v)
}

// removeText says what removing a cluster does: m is its registration
// (nil: it has not joined), set its settings at the hub (nil: none), js
// its join tokens.
func removeText(m *FleetMember, set *fleet.Settings, js []fleet.JoinToken, now time.Time) string {
	var parts []string
	switch {
	case m != nil && m.State == "connected":
		parts = append(parts, "Closes its link now")
	case m != nil:
		parts = append(parts, "Takes it out of the fleet: its key no longer connects")
	}
	unused := 0
	for _, j := range js {
		if j.Active(now) {
			unused++
		}
	}
	if unused > 0 {
		parts = append(parts, "voids its "+unitN(unused, "unused join token"))
	}
	kept := 0
	if set != nil {
		kept = len(set.Secrets)
		if kept > 0 {
			parts = append(parts, "forgets its settings and "+unitN(kept, "kept secret"))
		} else {
			parts = append(parts, "forgets its settings")
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "Forgets it")
	}
	text := parts[0]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			text += " and " + p
		} else {
			text += ", " + p
		}
	}
	text = strings.ToUpper(text[:1]) + text[1:] + "."
	if m != nil {
		text += " Its builds stay in the cluster."
	}
	if kept > 0 {
		text += " Joining again needs those secrets entered anew."
	}
	return text
}

// anyActive says whether any of js can still be traded at now.
func anyActive(js []fleet.JoinToken, now time.Time) bool {
	for _, j := range js {
		if j.Active(now) {
			return true
		}
	}
	return false
}

// clusterState is what the hub has of a cluster name: its registration
// (zero when none), its settings (Values nil when none), whether a join
// token for it is still valid. found: any of them.
func (s *Server) clusterState(r *http.Request, name string) (m fleet.Member, set fleet.Settings, joining, found bool, err error) {
	reg := s.opt.Hub.Registry
	ctx := r.Context()
	if m, err = reg.Get(ctx, name); errors.Is(err, fleet.ErrNotFound) || (err == nil && !m.RemovedAt.IsZero()) {
		m = fleet.Member{}
	} else if err != nil {
		return
	}
	if set, err = reg.Settings(ctx, name); errors.Is(err, fleet.ErrNotFound) {
		set = fleet.Settings{}
	} else if err != nil {
		return
	}
	js, err := reg.JoinTokens(ctx, name)
	if err != nil {
		return
	}
	joining = anyActive(js, s.now())
	return m, set, joining, m.Name != "" || set.Values != nil || joining, nil
}

// joinedBy is how a registered cluster without settings at the hub joined:
// by its key (registered through the API: its key's Secret is fleet-key),
// or with a join token (its controller made and keeps its key).
func joinedBy(m fleet.Member) string {
	if m.Origin == fleet.OriginJoin {
		return "token"
	}
	return "key"
}

func (s *Server) editCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Settings are changed by operators and owners.")
		return
	}
	name := r.PathValue("name")
	m, set, _, found, err := s.clusterState(r, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		s.notFound(w, r, "cluster")
		return
	}
	if set.Values == nil {
		// A cluster registered through the API, or a name with only a join
		// token: its settings start here.
		set = fleet.Settings{Member: name, Values: map[string]string{}}
		if m.Name != "" {
			set.Values["joins"] = joinedBy(m)
		}
	}
	s.clusterForm(w, r, name, true, set.Values, set.Kept(), nil, "", http.StatusOK)
}

func (s *Server) changeCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Settings are changed by operators and owners.")
		return
	}
	name := r.PathValue("name")
	reg := s.opt.Hub.Registry
	m, old, _, found, err := s.clusterState(r, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !found {
		s.notFound(w, r, "cluster")
		return
	}
	values := formValues(r)
	// How it joined is not a setting a change makes.
	delete(values, "joins")
	delete(values, "public_key")
	delete(values, "token_hours")
	switch {
	case old.Values != nil:
		values["joins"] = old.Values["joins"]
		if pk := old.Values["public_key"]; pk != "" {
			values["public_key"] = pk
		}
		if th := old.Values["token_hours"]; th != "" {
			values["token_hours"] = th
		}
	case m.Name != "":
		// Registered without settings (through the API): as it joined, or
		// the values would make it join otherwise and lose its key.
		values["joins"] = joinedBy(m)
		if values["joins"] == "key" {
			values["public_key"] = m.PublicKey
		}
	}
	set, err := reg.SaveSettings(r.Context(), s.opt.Hub.Sealer, s.opt.Hub.Who(r).Subject, name, values)
	var se *fleet.SettingsError
	if errors.As(err, &se) {
		s.clusterForm(w, r, name, true, values, old.Kept(), se.Fields, "", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.clusterForm(w, r, name, true, values, old.Kept(), nil, errText(err), http.StatusUnprocessableEntity)
		return
	}
	if s.opt.Hub.Deliver != nil {
		s.opt.Hub.Deliver(name)
	}
	// A cluster that has not joined yet is installed, not upgraded: its
	// commands, and a join token from its page.
	joined := m.Name != ""
	v := InstallPage{Name: name, Install: fleet.Generate(set, s.opt.Hub.Info, s.opt.Hub.Version, "", joined),
		Waiting: !joined && set.Values["joins"] != "key"}
	for _, fm := range s.opt.Fleet.Members() {
		v.Connected = v.Connected || (fm.Name == name && fm.State == "connected")
	}
	s.installPage(w, r, v)
}

// newJoin issues a new join token for a cluster that has not joined yet:
// the install page again, with the new token.
func (s *Server) newJoin(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Join tokens are issued by operators and owners.")
		return
	}
	name := r.PathValue("name")
	reg := s.opt.Hub.Registry
	set, err := reg.Settings(r.Context(), name)
	if err != nil {
		s.back(w, r, "/clusters/"+name, "This cluster has no settings at the hub: issue its token through the API.", true)
		return
	}
	// One still unused stops working: asked once more.
	if !confirmed(r) && s.confirmJoin(w, r, name) {
		return
	}
	j, secret, err := reg.IssueJoin(r.Context(), s.opt.Hub.Who(r).Subject, name, joinHours(set.Values))
	if err != nil {
		s.back(w, r, "/clusters/"+name, errText(err), true)
		return
	}
	s.installPage(w, r, InstallPage{Name: name, Expires: stampOf(j.ExpiresAt, zone(r), "Jan 2 15:04"),
		Install: fleet.Generate(set, s.opt.Hub.Info, s.opt.Hub.Version, secret, false)})
}

// removeCluster takes a cluster out of the fleet: its link closes, its
// record is kept a while; a cluster still waiting to join is forgotten.
func (s *Server) removeCluster(w http.ResponseWriter, r *http.Request) {
	if !s.canManage(r) {
		s.forbidden(w, r, "Clusters are removed by operators and owners.")
		return
	}
	name := r.PathValue("name")
	reg := s.opt.Hub.Registry
	who := s.opt.Hub.Who(r).Subject
	ctx := r.Context()
	if _, _, _, found, err := s.clusterState(r, name); err != nil {
		s.fail(w, err)
		return
	} else if !found {
		s.back(w, r, "/", "No cluster named "+name+" is in the fleet or waiting to join: nothing was removed.", true)
		return
	}
	// The name typed on the cluster's page, checked here: a form sent
	// without it removes nothing.
	switch typed := strings.TrimSpace(r.PostFormValue("name")); {
	case typed == "":
		s.back(w, r, "/clusters/"+name, "Not removed: type "+name+" to confirm.", true)
		return
	case typed != name:
		if len(typed) > 60 {
			typed = strings.ToValidUTF8(typed[:60], "") + "…"
		}
		s.back(w, r, "/clusters/"+name, "Not removed: the name typed was \""+typed+"\", not \""+name+"\".", true)
		return
	}
	// The member, its unused join tokens, its settings and secrets: one
	// transaction, as the API removes it.
	removed, err := reg.RemoveCluster(ctx, who, name)
	if err != nil && !errors.Is(err, fleet.ErrNotFound) {
		s.back(w, r, "/clusters/"+name, errText(err), true)
		return
	}
	if removed && s.opt.Hub.Removed != nil {
		s.opt.Hub.Removed(name)
	}
	s.back(w, r, "/", name+" is removed from the fleet. Its builds stay in the cluster; uninstall the box there, or join it again.", false)
}

// pending are the clusters that have not joined yet: with settings, or
// with a join token that can still be traded.
func (s *Server) pending(r *http.Request, members []FleetMember) []string {
	if s.opt.Hub == nil {
		return nil
	}
	sets, err := s.opt.Hub.Registry.SettingsList(r.Context())
	if err != nil {
		s.log.Warn("fleet: the clusters' settings", "err", err)
		return nil
	}
	joins, err := s.opt.Hub.Registry.ActiveJoins(r.Context())
	if err != nil {
		s.log.Warn("fleet: the join tokens", "err", err)
	}
	in := map[string]bool{}
	for _, m := range members {
		if m.State != "removed" {
			in[m.Name] = true
		}
	}
	var out []string
	add := func(name string) {
		if !in[name] {
			in[name] = true
			out = append(out, name)
		}
	}
	for _, set := range sets {
		add(set.Member)
	}
	for _, j := range joins {
		add(j.Member)
	}
	sort.Strings(out)
	return out
}
