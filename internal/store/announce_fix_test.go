package store

import (
	"testing"
	"time"
)

// Reading the same pipeline from GitLab again announces nothing; a change
// of the pipeline or of a job does.
func TestSaveAPIPipelineAnnouncesChanges(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	sub := s.Subscribe()
	defer s.Unsubscribe(sub)
	job := APIJob{ID: 70, Name: "build", Stage: "build", Status: "running", Started: now.Add(-time.Minute)}
	save := func(status string, read time.Time, j APIJob) {
		t.Helper()
		if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: "000001", ID: 5, ProjectID: 42, Status: status, Read: read, Jobs: []APIJob{j}}); err != nil {
			t.Fatal(err)
		}
	}
	heard := func() bool {
		select {
		case <-sub.C:
			return len(sub.Take()) > 0
		default:
			return false
		}
	}
	save("running", now, job)
	if !heard() {
		t.Error("a new pipeline was not announced")
	}
	save("running", now.Add(10*time.Second), job)
	if heard() {
		t.Error("a read that changed nothing was announced")
	}
	save("success", now.Add(20*time.Second), job)
	if !heard() {
		t.Error("the pipeline's status changed unannounced")
	}
	job.Status = "success"
	save("success", now.Add(30*time.Second), job)
	if !heard() {
		t.Error("a job's status changed unannounced")
	}
	save("success", now.Add(40*time.Second), job)
	if heard() {
		t.Error("a second read that changed nothing was announced")
	}
}
