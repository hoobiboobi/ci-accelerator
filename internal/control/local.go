// Package control connects the console's pages to what they read.
//
// The control plane runs as roles of one binary: the controller owns the
// store, the collector, the job-pod watch, the sessions and every GitLab
// token; the web serves the pages and holds nothing. In one process (the
// role `all`, and the tests) the pages read the controller's parts
// directly through Local, LocalGitLab and the sign-in itself. Split, the
// web reads them through a Client, which calls the controller's Server
// over HTTPS; the controller does every write the pages cause.
package control

import (
	"context"
	"errors"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Live is the daemons as the collector sees them now.
type Live interface {
	Daemons() []collector.DaemonState
}

// Local serves the pages' reads from the store and the collector of this
// process.
type Local struct {
	*store.Store
	// Live may be nil: no daemons known.
	Live Live
}

// Daemons is the collector's view of the daemons; nil without one.
func (l *Local) Daemons() []collector.DaemonState {
	if l.Live == nil {
		return nil
	}
	return l.Live.Daemons()
}

// LiveFollower follows running builds' progress (the collector does).
type LiveFollower interface {
	LiveBuild(ctx context.Context, daemon, ref string) (*collector.LiveBuild, error)
}

// LiveBuild is a running build of daemon as its page shows it; nil when
// the daemon runs no such build (any more), or nothing follows builds.
func (l *Local) LiveBuild(ctx context.Context, daemon, ref string) (*collector.LiveBuild, error) {
	f, ok := l.Live.(LiveFollower)
	if !ok {
		return nil, nil
	}
	return f.LiveBuild(ctx, daemon, ref)
}

// ErrNoToken: the request has no GitLab token to read with.
var ErrNoToken = errors.New("control: no GitLab token for this request")

// LocalGitLab reads GitLab for the pages in this process, with the token
// of the person the request is for, or the installation's.
type LocalGitLab struct {
	Fetcher *gitlab.Fetcher
	// Token is the installation's; nil: none. Not used with sign-in.
	Token gitlab.Credential
	// Auth is the sign-in; nil: none.
	Auth *auth.Auth
}

// Credential is whose token a request reads GitLab with: with sign-in the
// person's (the viewer in ctx, or the one whose session cookie ctx
// carries), else the installation's; nil: none.
func (g *LocalGitLab) Credential(ctx context.Context) gitlab.Credential {
	if g.Auth == nil {
		if g.Token == nil {
			return nil
		}
		return g.Token
	}
	if v := auth.From(ctx); v != nil && v.Usable() {
		return v
	}
	if raw := auth.CookieFrom(ctx); raw != "" {
		if v, _, err := g.Auth.LoadCookie(ctx, raw); err == nil && v != nil {
			return v
		}
	}
	return nil
}

// Knows says whether instance is one of the console's GitLabs.
func (g *LocalGitLab) Knows(instance string) bool { return g.Fetcher.Client.Knows(instance) }

// Ensure reads a pipeline's jobs when that is due.
func (g *LocalGitLab) Ensure(ctx context.Context, instance string, project, pipeline uint64, status string, read time.Time, wait time.Duration) {
	if cred := g.Credential(ctx); cred != nil {
		g.Fetcher.Ensure(cred, instance, project, pipeline, status, read, wait)
	}
}

// JobLogPart reads part of a job's log.
func (g *LocalGitLab) JobLogPart(ctx context.Context, instance string, project, job uint64, q gitlab.LogQuery) (gitlab.LogPart, error) {
	cred := g.Credential(ctx)
	if cred == nil {
		return gitlab.LogPart{}, ErrNoToken
	}
	return g.Fetcher.JobLogPart(ctx, cred, instance, project, job, q)
}

// Changed lists the files changed between two commits.
func (g *LocalGitLab) Changed(ctx context.Context, instance string, project uint64, from, to string) ([]string, bool, error) {
	cred := g.Credential(ctx)
	if cred == nil {
		return nil, false, ErrNoToken
	}
	return g.Fetcher.Changed(ctx, cred, instance, project, from, to)
}

// Status is the installation's standing with GitLab's rate limits.
func (g *LocalGitLab) Status() (time.Time, error) { return g.Fetcher.Status() }
