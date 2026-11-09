package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// A build whose only executed steps are git sources fetched again (moby's
// binary target, its last step received from another job) is stored with
// no step run: the lists say "every step came from the cache", not "1
// step ran; nothing to compare with".
func TestFetchedSourcesAreNotStepsRun(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Date(2026, 11, 9, 21, 0, 0, 0, time.UTC)})
	d := "../history/testdata/moby/2l9nh7i2965vnggagtocjx2vf"
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(d, n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.NewBuild(rec, progress, [][]byte{read("provenance.json")}, nil, nil, history.Config{SecretKey: s.SecretKey()})
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = "daemon-a"
	fetched := 0
	for _, st := range b.Steps {
		if st.State == history.Built && !strings.HasPrefix(st.Name, "FROM ") {
			fetched++
		}
	}
	if fetched == 0 {
		t.Fatal("the record shows no git source fetched again")
	}
	if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	var built int
	if err := s.db.QueryRowContext(ctx, "SELECT built FROM builds WHERE ref = ?", b.Ref).Scan(&built); err != nil {
		t.Fatal(err)
	}
	if built != 0 {
		t.Errorf("stored with %d steps run, want 0 (%d git sources fetched again)", built, fetched)
	}
}
