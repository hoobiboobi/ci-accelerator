package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Step output is a key -> value table in one SQLite file per UTC day:
//   - key: the build id and the step index, packed into the row id;
//   - value: the output, zstd-compressed, usually with a dictionary.
//
// Output of one step is small and compresses poorly on its own; with the
// same project's recent output as a dictionary it compresses about four
// times better. Each file holds the dictionaries its blocks use, built from
// a project's latest output when it first writes that day, never changed
// afterwards, so deleting an old file never breaks a newer one.

const (
	// stepBits is the room for the step index in an output key.
	stepBits = 12
	// MaxSteps is the most steps of one build that keep output.
	MaxSteps = 1 << stepBits
	// dictSize bounds a dictionary: zstd looks back at most this far.
	dictSize = 64 << 10
	// dictMin is the least earlier output worth a dictionary.
	dictMin = 4 << 10
)

// dictRetries are the numbers of writes without a dictionary after which a
// project tries again to build one from that day's own output.
var dictRetries = map[int]bool{8: true, 32: true, 128: true}

func outputKey(buildID int64, step int) int64 { return buildID<<stepBits | int64(step) }

func day(t time.Time) int {
	t = t.UTC()
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

func outputName(day int) string { return fmt.Sprintf("output-%d.db", day) }

type outputFile struct {
	day int
	db  *sql.DB

	mu      sync.Mutex
	dicts   map[string]*dictState // by project
	enc     map[int64]*zstd.Encoder
	dec     *zstd.Decoder
	decDict map[int64]bool
	plain   *zstd.Encoder
}

type dictState struct {
	id     int64 // 0: none yet
	writes int   // writes without one
}

// output opens (and with create, creates) the file of a day.
func (s *Store) output(ctx context.Context, d int, create bool) (*outputFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.outputs[d]; ok {
		return f, nil
	}
	path := filepath.Join(s.dir, outputName(d))
	if !create {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, outputSchema); err != nil {
		db.Close()
		return nil, err
	}
	plain, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil)
	f := &outputFile{day: d, db: db, dicts: map[string]*dictState{}, enc: map[int64]*zstd.Encoder{}, dec: dec, decDict: map[int64]bool{}, plain: plain}
	s.outputs[d] = f
	return f, nil
}

func (f *outputFile) close() error {
	f.dec.Close()
	return f.db.Close()
}

// writeOutputs stores the outputs of one build's steps (index -> output) in
// the file of day d. project groups dictionaries ("" for builds of no known
// project).
func (s *Store) writeOutputs(ctx context.Context, d int, buildID int64, project string, outputs map[int]string) error {
	if len(outputs) == 0 {
		return nil
	}
	f, err := s.output(ctx, d, true)
	if err != nil {
		return err
	}
	idx := make([]int, 0, len(outputs))
	for i := range outputs {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	f.mu.Lock()
	defer f.mu.Unlock()
	dict, err := s.dictFor(ctx, f, project)
	if err != nil {
		return err
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, i := range idx {
		if i >= MaxSteps {
			continue
		}
		enc := f.plain
		var dictID any
		if dict != 0 {
			enc, dictID = f.enc[dict], dict
		}
		data := enc.EncodeAll([]byte(outputs[i]), nil)
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO outputs (key, dict_id, data) VALUES (?, ?, ?)", outputKey(buildID, i), dictID, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dictFor returns the dictionary a project's next blocks in f use, building
// one when the project has none in f yet: from its latest output, in this
// file and the earlier ones, newest last (zstd matches nearest best).
func (s *Store) dictFor(ctx context.Context, f *outputFile, project string) (int64, error) {
	st := f.dicts[project]
	if st == nil {
		st = &dictState{}
		f.dicts[project] = st
		err := f.db.QueryRowContext(ctx, "SELECT id FROM dicts WHERE project = ? ORDER BY id DESC LIMIT 1", project).Scan(&st.id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if st.id != 0 {
			return st.id, f.loadDict(ctx, st.id)
		}
	} else if st.id != 0 {
		return st.id, nil
	} else if st.writes++; !dictRetries[st.writes] {
		return 0, nil
	}
	content, err := s.recentOutput(ctx, project, f)
	if err != nil || len(content) < dictMin {
		return 0, err
	}
	stored := f.plain.EncodeAll(content, nil)
	res, err := f.db.ExecContext(ctx, "INSERT INTO dicts (project, data) VALUES (?, ?)", project, stored)
	if err != nil {
		return 0, err
	}
	if st.id, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	return st.id, f.useDict(st.id, content)
}

func (f *outputFile) loadDict(ctx context.Context, id int64) error {
	if f.enc[id] != nil {
		return nil
	}
	var stored []byte
	if err := f.db.QueryRowContext(ctx, "SELECT data FROM dicts WHERE id = ?", id).Scan(&stored); err != nil {
		return err
	}
	content, err := f.dec.DecodeAll(stored, nil)
	if err != nil {
		return err
	}
	return f.useDict(id, content)
}

func (f *outputFile) useDict(id int64, content []byte) error {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderDictRaw(uint32(id), content))
	if err != nil {
		return err
	}
	f.enc[id] = enc
	if f.decDict[id] {
		return nil
	}
	// A decoder knows a fixed set of dictionaries: rebuild it with one more.
	opts := []zstd.DOption{zstd.WithDecoderDictRaw(uint32(id), content)}
	for other := range f.decDict {
		var stored []byte
		if err := f.db.QueryRow("SELECT data FROM dicts WHERE id = ?", other).Scan(&stored); err != nil {
			return err
		}
		c, err := f.dec.DecodeAll(stored, nil)
		if err != nil {
			return err
		}
		opts = append(opts, zstd.WithDecoderDictRaw(uint32(other), c))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		return err
	}
	f.dec.Close()
	f.dec = dec
	f.decDict[id] = true
	return nil
}

// recentOutput collects a project's latest step output, up to dictSize,
// newest last: from the builds the main database lists for the project, in
// the files that still exist.
func (s *Store) recentOutput(ctx context.Context, project string, current *outputFile) ([]byte, error) {
	instance, projectID, ok := splitProject(project)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.output_day, s.idx FROM builds b JOIN steps s ON s.build_id = b.id
		WHERE b.instance = ? AND b.project_id = ? AND b.output_day IS NOT NULL AND s.output_len > 0
		ORDER BY b.id DESC, s.idx DESC LIMIT 256`, instance, projectID)
	if err != nil {
		return nil, err
	}
	type ref struct {
		build int64
		day   int
		step  int
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.build, &r.day, &r.step); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	var chunks [][]byte
	total := 0
	for _, r := range refs {
		if total >= dictSize {
			break
		}
		f := current
		if r.day != current.day {
			if f, err = s.output(ctx, r.day, false); err != nil {
				continue // deleted or never written
			}
			f.mu.Lock()
		}
		out, err := f.read(ctx, outputKey(r.build, r.step))
		if f != current {
			f.mu.Unlock()
		}
		if err != nil || len(out) == 0 {
			continue
		}
		chunks = append(chunks, out)
		total += len(out)
	}
	var content []byte
	for i := len(chunks) - 1; i >= 0; i-- { // oldest first, newest last
		content = append(content, chunks[i]...)
	}
	if len(content) > dictSize {
		content = content[len(content)-dictSize:]
	}
	return content, nil
}

// read returns one block, decompressed; f.mu must be held.
func (f *outputFile) read(ctx context.Context, key int64) ([]byte, error) {
	var dictID sql.NullInt64
	var data []byte
	if err := f.db.QueryRowContext(ctx, "SELECT dict_id, data FROM outputs WHERE key = ?", key).Scan(&dictID, &data); err != nil {
		return nil, err
	}
	if dictID.Valid {
		if err := f.loadDict(ctx, dictID.Int64); err != nil {
			return nil, err
		}
	}
	return f.dec.DecodeAll(data, nil)
}

// Output returns what a step printed, or "" when nothing is kept: the step
// printed nothing, or its day's file is past retention.
func (s *Store) Output(ctx context.Context, buildID int64, step int) (string, error) {
	var d sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT output_day FROM builds WHERE id = ?", buildID).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !d.Valid) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	f, err := s.output(ctx, int(d.Int64), false)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out, err := f.read(ctx, outputKey(buildID, step))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return string(out), err
}

// pruneOutputs deletes the output files older than the retention.
func (s *Store) pruneOutputs(now time.Time) error {
	oldest := day(now.AddDate(0, 0, -(s.opts.KeepOutput - 1)))
	names, err := filepath.Glob(filepath.Join(s.dir, "output-*.db"))
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		d, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(n), "output-"), ".db"))
		if err != nil || d >= oldest {
			continue
		}
		s.mu.Lock()
		if f, ok := s.outputs[d]; ok {
			errs = append(errs, f.close())
			delete(s.outputs, d)
		}
		s.mu.Unlock()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(n + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func projectKey(instance, projectID sql.NullInt64) string {
	if !instance.Valid || !projectID.Valid {
		return ""
	}
	return fmt.Sprintf("%d:%d", instance.Int64, projectID.Int64)
}

func splitProject(key string) (int64, int64, bool) {
	a, b, ok := strings.Cut(key, ":")
	if !ok {
		return 0, 0, false
	}
	i, err1 := strconv.ParseInt(a, 10, 64)
	p, err2 := strconv.ParseInt(b, 10, 64)
	return i, p, err1 == nil && err2 == nil
}
