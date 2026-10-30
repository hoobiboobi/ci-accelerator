package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// buildProject is the project stored with a build, 0 for none.
func buildProject(t *testing.T, s *Store, id int64) uint64 {
	t.Helper()
	var p sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT project_id FROM builds WHERE id = ?", id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return uint64(p.Int64)
}

// A build's trace id is chosen by the job that runs buildctl: one that
// names its own pipeline and another project's job takes nothing of that
// job's project, and has no previous build there to compare with.
func TestTraceNamingAnotherPipelinesJob(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	victim, forged := bs[7], bs[8] // api builds, pipelines 105 and 106
	saveJobs(t, s, bs[:8])
	for _, b := range bs[:8] {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	inst := victim.Attribution.Instance
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 9001, PipelineID: 900, ProjectID: 7, ProjectPath: "other/app"}); err != nil {
		t.Fatal(err)
	}
	forged.Attribution.Pipeline, forged.Attribution.Job = 900, victim.Attribution.Job
	if p, known, err := s.Previous(ctx, forged); err != nil || p != nil || known {
		t.Fatalf("previous of a build whose job is of another pipeline: %v, known %v, %v", p, known, err)
	}
	id, err := s.SaveBuild(ctx, Build{Build: forged, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p := buildProject(t, s, id); p != 0 {
		t.Fatalf("the build took project %d from a job of another pipeline", p)
	}
	// The job's pod seen again does not attach it either.
	saveJobs(t, s, []*history.Build{victim})
	if p := buildProject(t, s, id); p != 0 {
		t.Fatalf("the job's pod gave the build project %d", p)
	}
	rows, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: inst, ID: 42}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			t.Fatal("the build is in the feed of its job's project")
		}
	}
	// A build of the job's own pipeline still takes its project, and is
	// the job's only build (its log's link leads there).
	if p := buildProject(t, s, mustID(t, s, victim.Ref)); p != 42 {
		t.Fatalf("the job's own build: project %d", p)
	}
	if ids, err := s.JobBuilds(ctx, inst, victim.Attribution.Job); err != nil || len(ids) != 1 || ids[0] != mustID(t, s, victim.Ref) {
		t.Fatalf("builds of the job: %v, %v", ids, err)
	}
}

func mustID(t *testing.T, s *Store, ref string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM builds WHERE ref = ?", ref).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A pod's pipeline id comes from a CI variable the job can set: a job of
// another project that names a pipeline neither moves the pipeline to its
// project nor rewrites its commit, and is not listed among its jobs.
func TestJobOfAnotherProjectNamingAPipeline(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	const inst = "0a1b2c"
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 1001, PipelineID: 108, ProjectID: 42, Ref: "main", SHA: "aaaa", CommitTitle: "real"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 2001, PipelineID: 108, ProjectID: 7, Ref: "evil", SHA: "bbbb", CommitTitle: "pwned"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PipelineInfo(ctx, inst, 108)
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	if p.ProjectID != 42 || p.CommitTitle != "real" || p.Ref != "main" || p.SHA != "aaaa" {
		t.Fatalf("pipeline after another project's job: project %d, %q %q %q", p.ProjectID, p.CommitTitle, p.Ref, p.SHA)
	}
	if project, err := s.PipelineProject(ctx, inst, 108); err != nil || project != 42 {
		t.Fatalf("pipeline project %d, %v", project, err)
	}
	jobs, err := s.PipelineJobs(ctx, inst, 108)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 1001 {
		t.Fatalf("pipeline jobs: %+v", jobs)
	}
	// It keeps the pipeline it named: its builds naming another pipeline
	// take nothing of its project either.
	j, err := s.Job(ctx, inst, 2001)
	if err != nil || j == nil || j.ProjectID != 7 || j.PipelineID != 108 {
		t.Fatalf("the other project's job: %+v, %v", j, err)
	}
	// Its own project's jobs still update the pipeline.
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 1002, PipelineID: 108, ProjectID: 42, Ref: "main", SHA: "aaaa", CommitTitle: "real, retried"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PipelineInfo(ctx, inst, 108); p.CommitTitle != "real, retried" {
		t.Fatalf("pipeline not updated by its own job: %q", p.CommitTitle)
	}
}

// stepHistoryProjects returns the projects of the builds in a build's step
// histories, and the pipelines they ran in.
func stepHistoryProjects(t *testing.T, s *Store, id int64, steps int) (projects, pipelines map[uint64]int) {
	t.Helper()
	projects, pipelines = map[uint64]int{}, map[uint64]int{}
	for i := range steps {
		runs, err := s.StepHistory(ctx, id, i, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			projects[buildProject(t, s, r.BuildID)]++
			pipelines[r.Pipeline]++
		}
	}
	return projects, pipelines
}

// saveSplit stores every build, the jobs of pipelines 104 and 105 as
// project 42's and every other job as project 77's, skipping the jobs in
// skip.
func saveSplit(t *testing.T, s *Store, bs []*history.Build, skip map[uint64]bool) {
	t.Helper()
	for _, b := range bs {
		a := b.Attribution
		if a.State != history.Attributed || skip[a.Job] {
			continue
		}
		project := uint64(77)
		if a.Pipeline == 104 || a.Pipeline == 105 {
			project = 42
		}
		if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, ProjectID: project}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

// A job whose pod was never seen (it ended while the controller was away)
// is learnt from GitLab's API: its build takes the project then, and is
// listed with its pipeline.
func TestJobLearntFromGitLabGivesItsBuildAProject(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	v := bs[4] // api, pipeline 104 with two more jobs
	saveSplit(t, s, bs, map[uint64]bool{v.Attribution.Job: true})
	id := mustID(t, s, v.Ref)
	read := APIPipeline{Instance: v.Attribution.Instance, ID: 104, ProjectID: 42, Status: "success",
		Jobs: []APIJob{{ID: v.Attribution.Job, Name: "build-api", Status: "success"}}, Read: c.t}
	attached, err := s.SaveAPIPipeline(ctx, read)
	if err != nil {
		t.Fatal(err)
	}
	if p := buildProject(t, s, id); p != 42 {
		t.Fatalf("build of a job learnt from GitLab: project %d", p)
	}
	// It says which job's builds are to be compared now, once.
	if len(attached) != 1 || attached[0] != v.Attribution.Job {
		t.Fatalf("jobs whose builds took a project: %v", attached)
	}
	if attached, err := s.SaveAPIPipeline(ctx, read); err != nil || len(attached) != 0 {
		t.Fatalf("read again: %v, %v", attached, err)
	}
	rows, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: v.Attribution.Instance, ID: 42}, Pipeline: 104})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		found = found || r.ID == id
	}
	if !found {
		t.Fatal("the build is not listed with its pipeline")
	}
	if projects, _ := stepHistoryProjects(t, s, id, len(v.Steps)); projects[77] > 0 || projects[42] == 0 {
		t.Fatalf("step history by project: %v", projects)
	}
}

// A build of no known project never lists other projects' runs in its step
// history: it keeps to its pipeline's project, or to its pipeline when
// that is not known either. Only a build of no CI job sees every build.
func TestStepHistoryOfABuildWithoutProject(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	inPipeline, alone, none := bs[4], bs[0], bs[10] // pipeline 104 (other jobs known), 100 (no other job), no trace
	if none.Attribution.State == history.Attributed {
		t.Fatal("fixture 10 is attributed")
	}
	saveSplit(t, s, bs, map[uint64]bool{inPipeline.Attribution.Job: true, alone.Attribution.Job: true})
	// Its own runs (no project) are among them.
	projects, pipelines := stepHistoryProjects(t, s, mustID(t, s, inPipeline.Ref), len(inPipeline.Steps))
	if projects[77] > 0 || projects[42] == 0 || pipelines[105] == 0 || projects[0] != len(inPipeline.Steps) {
		t.Errorf("job not known, pipeline of project 42: step history by project %v, by pipeline %v", projects, pipelines)
	}
	_, pipelines = stepHistoryProjects(t, s, mustID(t, s, alone.Ref), len(alone.Steps))
	if len(pipelines) != 1 || pipelines[100] == 0 {
		t.Errorf("job and pipeline not known: step history by pipeline %v", pipelines)
	}
	projects, _ = stepHistoryProjects(t, s, mustID(t, s, none.Ref), len(none.Steps))
	if projects[77] == 0 || projects[42] == 0 {
		t.Errorf("build of no job: step history by project %v", projects)
	}
}

// A job can name a pipeline that does not exist yet (its id is a CI
// variable, and GitLab's ids are sequential): the pipeline is that job's
// project's until GitLab says whose it is. Meanwhile the real jobs keep
// the pipeline they named, so a trace naming one of them with another
// pipeline takes nothing of their project; once GitLab answers for the
// real project, the pipeline, its jobs and builds are that project's, and
// the claiming job's are nobody's.
func TestPipelineClaimedBeforeItExists(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	victim := bs[7] // api, pipeline 105
	a := victim.Attribution
	if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: 9001, PipelineID: a.Pipeline, ProjectID: 7, ProjectPath: "other/app",
		Ref: "evil", SHA: "bbbb", CommitTitle: "claimed"}); err != nil {
		t.Fatal(err)
	}
	claim := *bs[8]
	claim.Ref, claim.Attribution.Pipeline, claim.Attribution.Job = "buildoftheclaimingjob0000", a.Pipeline, 9001
	claimID, err := s.SaveBuild(ctx, Build{Build: &claim, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	saveJobs(t, s, []*history.Build{victim})
	id, err := s.SaveBuild(ctx, Build{Build: victim, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if j, err := s.Job(ctx, a.Instance, a.Job); err != nil || j == nil || j.PipelineID != a.Pipeline || j.ProjectID != 42 {
		t.Fatalf("the real job: %+v, %v", j, err)
	}
	forged := *bs[8]
	forged.Ref, forged.Attribution.Pipeline, forged.Attribution.Job = "forgedtraceofthevictimjob", 777, a.Job
	if p, known, err := s.Previous(ctx, &forged); err != nil || p != nil || known {
		t.Fatalf("previous of a build naming the job with another pipeline: %v, known %v, %v", p, known, err)
	}
	forgedID, err := s.SaveBuild(ctx, Build{Build: &forged, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p := buildProject(t, s, forgedID); p != 0 {
		t.Fatalf("a build naming the job with another pipeline took project %d", p)
	}
	// GitLab answers for project 42.
	if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: a.Instance, ID: a.Pipeline, ProjectID: 42, Status: "success",
		Jobs: []APIJob{{ID: a.Job, Name: "build-api", Status: "success"}}, Read: c.t}); err != nil {
		t.Fatal(err)
	}
	if project, err := s.PipelineProject(ctx, a.Instance, a.Pipeline); err != nil || project != 42 {
		t.Fatalf("pipeline project after GitLab answered: %d, %v", project, err)
	}
	if p := buildProject(t, s, id); p != 42 {
		t.Fatalf("the real job's build: project %d", p)
	}
	if p := buildProject(t, s, claimID); p != 0 {
		t.Fatalf("the claiming job's build: project %d", p)
	}
	if p := buildProject(t, s, forgedID); p != 0 {
		t.Fatalf("the forged build: project %d", p)
	}
	p, err := s.PipelineInfo(ctx, a.Instance, a.Pipeline)
	if err != nil || p == nil || p.Ref == "evil" || p.SHA == "bbbb" || p.CommitTitle == "claimed" {
		t.Fatalf("the pipeline keeps what the claiming job said: %+v, %v", p, err)
	}
	jobs, err := s.PipelineJobs(ctx, a.Instance, a.Pipeline)
	if err != nil || len(jobs) != 1 || jobs[0].ID != a.Job {
		t.Fatalf("pipeline jobs: %+v, %v", jobs, err)
	}
	if n, err := s.PipelineJobCount(ctx, a.Instance, a.Pipeline); err != nil || n != 1 {
		t.Fatalf("pipeline job count: %d, %v", n, err)
	}
	// Nor does a later build of the claiming job.
	again := claim
	again.Ref = "laterbuildoftheclaimingjob"
	if againID, err := s.SaveBuild(ctx, Build{Build: &again, Generation: 1}); err != nil || buildProject(t, s, againID) != 0 {
		t.Fatalf("a later build of the claiming job: project %d, %v", buildProject(t, s, againID), err)
	}
	// The claiming job's pod seen again changes nothing.
	if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: 9001, PipelineID: a.Pipeline, ProjectID: 7, Ref: "evil"}); err != nil {
		t.Fatal(err)
	}
	if project, _ := s.PipelineProject(ctx, a.Instance, a.Pipeline); project != 42 || buildProject(t, s, claimID) != 0 {
		t.Fatalf("after the claiming job's pod: pipeline project %d, its build's %d", project, buildProject(t, s, claimID))
	}
}
