package web

import (
	"context"
	"net/http"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The pages read everything through three interfaces: in one process
// they are the store, the GitLab reader and the sign-in themselves; in
// the web process of a split console they are a client of the controller,
// which owns the store and every GitLab token. The pages never write
// anything themselves: the few writes they cause (GitLab's answers,
// settled reasons, sessions) are done by whoever implements these.

// Backend is what the pages read: the store's queries and the daemons'
// live state.
type Backend interface {
	Projects(ctx context.Context) ([]store.Project, error)
	Builds(ctx context.Context, q store.BuildQuery) ([]store.BuildRow, error)
	Branches(ctx context.Context, p store.ProjectKey, since time.Time) ([]string, error)
	Targets(ctx context.Context, p store.ProjectKey, since time.Time) ([]string, error)
	StepCounts(ctx context.Context, p store.ProjectKey, target string, since time.Time, limit int) ([]store.StepCount, error)
	ImageDays(ctx context.Context, p store.ProjectKey, target string, since time.Time) ([]store.ImageDay, error)
	PipelineList(ctx context.Context, q store.PipeQuery) ([]store.PipeRow, error)
	JobsOf(ctx context.Context, instance string, pipelines []uint64) (map[uint64][]store.JobInfo, error)
	BuildsOf(ctx context.Context, p store.ProjectKey, pipelines []uint64) (map[uint64][]store.BuildRow, error)
	PipelineInfo(ctx context.Context, instance string, id uint64) (*store.Pipeline, error)
	PipelineProject(ctx context.Context, instance string, pipeline uint64) (uint64, error)
	PipelineJobs(ctx context.Context, instance string, pipeline uint64) ([]store.JobInfo, error)
	Job(ctx context.Context, instance string, id uint64) (*store.JobInfo, error)
	JobBuilds(ctx context.Context, instance string, job uint64) ([]int64, error)
	BuildPage(ctx context.Context, id int64) (*store.BuildPage, error)
	Refs(ctx context.Context, refs []string) (map[string]store.RefInfo, error)
	Previous(ctx context.Context, b *history.Build) (*store.Stored, bool, error)
	Usual(ctx context.Context, b *history.Build, project uint64) (time.Duration, error)
	StepHistory(ctx context.Context, buildID int64, step int, limit int) ([]store.StepRun, error)
	Output(ctx context.Context, buildID int64, step int) (string, error)
	DaemonCounts(ctx context.Context, since time.Time) (map[string]store.DaemonCount, error)
	ProjectCache(ctx context.Context, p store.ProjectKey) ([]store.CacheObject, []store.CacheStore, error)
	Elsewhere(ctx context.Context, p store.ProjectKey, since time.Time, limit int) ([]store.Elsewhere, error)
	SettleElsewhere(ctx context.Context, settled []store.Settled) error
	// Recent and Ping serve the JSON API and the health check.
	Recent(ctx context.Context, limit int) ([]*store.Stored, error)
	Ping(ctx context.Context) error

	// Subscribe receives the pipelines that change ("" for the daemons'
	// live state, "*" for anything: the source of changes was away).
	Subscribe() *store.Subscription
	Unsubscribe(sub *store.Subscription)

	// Daemons is the build daemons as the collector sees them now.
	Daemons() []collector.DaemonState
}

// GitLab reads GitLab's API for the pages. Whose token it reads with
// comes with ctx: the person signed in (auth.From, or the session cookie
// for another process), else the installation's token.
type GitLab interface {
	// Knows says whether instance is one of the console's GitLabs.
	Knows(instance string) bool
	// Ensure reads a pipeline's jobs when that is due, waiting up to wait.
	Ensure(ctx context.Context, instance string, project, pipeline uint64, status string, read time.Time, wait time.Duration)
	JobLogPart(ctx context.Context, instance string, project, job uint64, q gitlab.LogQuery) (gitlab.LogPart, error)
	Changed(ctx context.Context, instance string, project uint64, from, to string) ([]string, bool, error)
	// Status is the installation's standing with GitLab's rate limits.
	Status() (until time.Time, err error)
}

// Signer signs people in through GitLab and says what they may see.
type Signer interface {
	PublicURL() string
	Instances() []auth.Choice
	Begin(w http.ResponseWriter, r *http.Request, instance, to string) error
	Finish(w http.ResponseWriter, r *http.Request, instance string) (string, error)
	Load(w http.ResponseWriter, r *http.Request) (*auth.Viewer, error)
	SignOut(w http.ResponseWriter, r *http.Request) error
	CanRead(ctx context.Context, v *auth.Viewer, instance string, project uint64) (bool, error)
}
