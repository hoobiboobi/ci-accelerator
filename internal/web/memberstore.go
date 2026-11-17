package web

import (
	"net/http"
	"strings"
)

// A cluster's store at a fleet hub. Build ids start again when a cluster
// comes back with another store (its controller's volume replaced), so the
// address of a build from before then would open another build. The hub's
// addresses of a cluster's pages carry the mark of its store
// (/m/<name>.<mark>/...); an address with the mark of another store says
// the build is gone instead. Addresses without a mark open what the store
// has now, as they always did.

// storeMarkLen is how many hex digits of a store id its mark is.
const storeMarkLen = 4

// StoreMark is a store id's mark in addresses: its first hex digits; ""
// for an id that is not one.
func StoreMark(store string) string {
	if len(store) < storeMarkLen {
		return ""
	}
	m := store[:storeMarkLen]
	for _, c := range m {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return m
}

// MemberBase is the base of a cluster's pages at a hub: /m/<name>.<mark>,
// or /m/<name> while its store is not known.
func MemberBase(name, store string) string {
	if m := StoreMark(store); m != "" {
		return "/m/" + name + "." + m
	}
	return "/m/" + name
}

// SplitMember is the segment after /m/ as the cluster's name and the mark
// of the store it carries ("" without one). A cluster's name has no dot.
func SplitMember(seg string) (name, mark string) {
	name, mark, _ = strings.Cut(seg, ".")
	return name, mark
}

// OldStore answers an address of a cluster's pages (/m/<seg>/...) that
// carries the mark of another store than the cluster's now, and says
// whether it did. A build's address says the build is gone (410); any
// other page leads to the same page of the cluster now. An address without
// a mark or with the mark of the store now, and a cluster the hub does not
// have (or has removed), are not answered here.
func (s *Server) OldStore(w http.ResponseWriter, r *http.Request, seg string) bool {
	name, mark := SplitMember(seg)
	if mark == "" || s.opt.Fleet == nil {
		return false
	}
	var m *FleetMember
	for _, fm := range s.opt.Fleet.Members() {
		if fm.Name == name && fm.State != "removed" {
			m = &fm
		}
	}
	if m == nil || StoreMark(m.Store) == "" || StoreMark(m.Store) == mark {
		return false
	}
	rest := "/"
	if esc := r.URL.EscapedPath(); strings.HasPrefix(esc, "/m/"+seg+"/") {
		rest = esc[len("/m/"+seg):]
	}
	build, _, _ := strings.Cut(strings.TrimPrefix(rest, "/builds/"), "/")
	if !strings.HasPrefix(rest, "/builds/") || build == "" {
		to := MemberBase(name, m.Store) + rest
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		// Through redirect, which sends only to a path of the site.
		s.redirect(w, r, to, http.StatusFound)
		return true
	}
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return true
	}
	sh.Title = "This build is gone"
	hint := "This address names a store that " + name + " does not have now: builds from it are not kept."
	if !m.StoreReplaced.IsZero() {
		hint = "The store of " + name + " was replaced on " + stampOf(m.StoreReplaced, zone(r), "Jan 2, 2006 15:04").Text +
			": builds from before then are not kept"
		if digits(build) {
			hint += ", and build " + build + " of its store now is another build"
		}
		hint += "."
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "notfound", http.StatusGone, NotFound{Shell: sh, What: "build", Hint: hint,
		Links: []Option{{Value: "/~/m/" + name + "/builds", Label: "Open " + name + "'s builds"}}})
	return true
}

// digits says whether s is a number written in decimal digits.
func digits(s string) bool {
	if s == "" || len(s) > 19 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
