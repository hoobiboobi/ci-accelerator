package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// Changes at the hub that take something away from someone (an API
// token revoked, a role changed or taken away, an unused join token
// replaced) are asked once more on one page that says what happens, and
// are made only from its form. The page's buttons are links to it (GET,
// ?confirm=...); a form sent without confirm=yes gets the same page, so
// nothing is changed by one click, with or without the page's script.
// Removing a cluster is asked on its own page: its name typed.

// ConfirmPage asks before a change.
type ConfirmPage struct {
	Shell
	// Crumbs: the way back, as label and address.
	Crumbs []Option
	// Question: the page's title; Lines: what happens.
	Question string
	Lines    []string
	// Action: where the form goes; Fields: its hidden fields; Button: its
	// label; Cancel: where Cancel leads.
	Action string
	Fields []Field
	Button string
	Cancel string
}

// Field is a hidden field of a form.
type Field struct{ Name, Value string }

// confirmed says whether a form was sent from the confirmation page.
func confirmed(r *http.Request) bool { return r.PostFormValue("confirm") == "yes" }

// confirm renders the confirmation page; section: the sidebar's.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request, section string, v ConfirmPage) {
	sh, err := s.shell(r, section, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = v.Question
	v.Shell = sh
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "confirm", http.StatusOK, v)
}

// accessConfirm answers GET /access?confirm=... and /access/tokens?confirm=...
// (the buttons of the Access page); false: no confirmation was asked for.
func (s *Server) accessConfirm(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	switch q.Get("confirm") {
	case "role":
		s.confirmRole(w, r, q.Get("subject"), q.Get("role"))
	case "remove-role":
		s.confirmRevokeRole(w, r, q.Get("subject"))
	case "revoke":
		s.confirmRevokeToken(w, r, q.Get("name"))
	default:
		return false
	}
	return true
}

// binding is subject's role binding at the hub.
func (s *Server) binding(r *http.Request, subject string) (fleet.Binding, bool, error) {
	bs, err := s.opt.Hub.Registry.Bindings(r.Context())
	if err != nil {
		return fleet.Binding{}, false, err
	}
	for _, b := range bs {
		if b.Subject == subject {
			return b, true, nil
		}
	}
	return fleet.Binding{}, false, nil
}

// roleCheck refuses what an owner may not change from the pages: not an
// owner with sign-in, their own role, an owner from the hub's values.
// false: it answered.
func (s *Server) roleCheck(w http.ResponseWriter, r *http.Request, subject string, b fleet.Binding, found bool) bool {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermRolesWrite) {
		s.forbidden(w, r, "Roles are changed by owners.")
		return false
	}
	if subject == p.Subject {
		s.back(w, r, "/access", "Your own role is changed by another owner.", true)
		return false
	}
	if found && b.Origin == fleet.OriginValues {
		s.back(w, r, "/access", person(s.opt.Hub.Registry, subject)+" is an owner from the hub's values: change it there.", true)
		return false
	}
	return true
}

// roleWords is what a role lets its holder do at the hub.
var roleWords = map[fleet.Role]string{
	fleet.Operator: "clusters, join tokens and the audit",
	fleet.Owner:    "clusters, join tokens, the audit, roles (other owners' too) and API tokens",
}

// confirmRole asks before a person's role changes to another.
func (s *Server) confirmRole(w http.ResponseWriter, r *http.Request, subject, to string) {
	b, found, err := s.binding(r, subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.roleCheck(w, r, subject, b, found) {
		return
	}
	role, err := fleet.ParseRole(to)
	if err != nil || !found || (role != fleet.Operator && role != fleet.Owner) {
		s.back(w, r, "/access", "Nothing to change: "+strings.TrimPrefix(subject, "gitlab:")+" has no role, or "+to+" is not one the pages give.", true)
		return
	}
	who := person(s.opt.Hub.Registry, subject)
	s.confirm(w, r, "access", ConfirmPage{
		Crumbs:   []Option{{Value: "/~/access", Label: "Access"}},
		Question: "Make " + who + " " + article(role) + "?",
		Lines: []string{
			who + " is " + article(b.Role) + " now: " + roleWords[b.Role] + ".",
			"As " + article(role) + " they have " + roleWords[role] + ", from their next page on.",
		},
		Action: "/~/access/people", Fields: []Field{{"subject", subject}, {"role", string(role)}},
		Button: "Make " + string(role), Cancel: "/~/access",
	})
}

// article is a role with its article.
func article(role fleet.Role) string {
	if role == fleet.Operator || role == fleet.Owner {
		return "an " + string(role)
	}
	return "a " + string(role)
}

// confirmRevokeRole asks before a person's role is taken away.
func (s *Server) confirmRevokeRole(w http.ResponseWriter, r *http.Request, subject string) {
	b, found, err := s.binding(r, subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.roleCheck(w, r, subject, b, found) {
		return
	}
	if !found {
		s.back(w, r, "/access", strings.TrimPrefix(subject, "gitlab:")+" has no role: nothing was changed.", true)
		return
	}
	who := person(s.opt.Hub.Registry, subject)
	s.confirm(w, r, "access", ConfirmPage{
		Crumbs:   []Option{{Value: "/~/access", Label: "Access"}},
		Question: "Remove the role of " + who + "?",
		Lines: []string{
			who + " is " + article(b.Role) + " now: " + roleWords[b.Role] + ".",
			"Without a role they see, cluster by cluster, only the projects GitLab lets them read, and change nothing at the hub, from their next page on.",
		},
		Action: "/~/access/people/remove", Fields: []Field{{"subject", subject}},
		Button: "Remove the role", Cancel: "/~/access",
	})
}

// confirmRevokeToken asks before an API token is revoked.
func (s *Server) confirmRevokeToken(w http.ResponseWriter, r *http.Request, name string) {
	p := s.opt.Hub.Who(r)
	if s.opt.Auth == nil || !p.Can(fleet.PermTokensWrite) {
		s.forbidden(w, r, "API tokens are revoked by owners.")
		return
	}
	ts, err := s.opt.Hub.Registry.Tokens(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var t *fleet.Token
	for i := range ts {
		if ts[i].Name == name && ts[i].Active(s.now()) {
			t = &ts[i]
		}
	}
	if t == nil {
		s.back(w, r, "/access/tokens", "No API token named "+name+" can be revoked: nothing was changed.", true)
		return
	}
	lines := []string{"Scripts and Terraform that use " + t.Name + " (…" + t.Hint + ", " + string(t.Role) + ") are refused by the hub's API from now on."}
	if !t.UsedAt.IsZero() {
		lines = append(lines, "It was last used "+agoAt(t.UsedAt, s.now()).Text+".")
	} else {
		lines = append(lines, "It was never used.")
	}
	lines = append(lines, "A revoked token does not open again: issue a new one and hand it out instead.")
	s.confirm(w, r, "access", ConfirmPage{
		Crumbs:   []Option{{Value: "/~/access", Label: "Access"}, {Value: "/~/access/tokens", Label: "API tokens"}},
		Question: "Revoke the token " + t.Name + "?", Lines: lines,
		Action: "/~/access/tokens/revoke", Fields: []Field{{"name", t.Name}},
		Button: "Revoke " + t.Name, Cancel: "/~/access/tokens",
	})
}

// confirmJoin asks before a new join token replaces the ones of a cluster
// that are still unused; false: none would be replaced (nothing to ask).
func (s *Server) confirmJoin(w http.ResponseWriter, r *http.Request, name string) bool {
	js, err := s.opt.Hub.Registry.JoinTokens(r.Context(), name)
	if err != nil {
		s.fail(w, err)
		return true
	}
	now := s.now()
	var lines []string
	for _, j := range js {
		if j.Active(now) {
			lines = append(lines, "Join token #"+strconv.FormatInt(j.ID, 10)+", issued "+agoAt(j.CreatedAt, now).Text+" by "+
				person(s.opt.Hub.Registry, j.CreatedBy)+", stops working: a cluster installing with it now does not join.")
		}
	}
	if len(lines) == 0 {
		return false
	}
	lines = append(lines, "The new token comes with the install commands on the next page.")
	s.confirm(w, r, "members", ConfirmPage{
		Crumbs:   []Option{{Value: "/~/", Label: "Members"}, {Value: "/~/clusters/" + name, Label: name}},
		Question: "A new join token for " + name + "?", Lines: lines,
		Action: "/~/clusters/" + name + "/join",
		Button: "Issue a new join token", Cancel: "/~/clusters/" + name,
	})
	return true
}
