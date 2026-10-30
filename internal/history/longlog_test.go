package history

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// progressBlob writes statuses as BuildKit's history does.
func progressBlob(t *testing.T, msgs []*controlapi.StatusResponse) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, m := range msgs {
		b, err := m.MarshalVT()
		if err != nil {
			t.Fatal(err)
		}
		binary.Write(&buf, binary.LittleEndian, uint32(len(b)))
		buf.Write(b)
	}
	return buf.Bytes()
}

// loudStep is a vertex whose log is written in pieces of size chunk.
func loudStep(i int, log string, chunk int) []*controlapi.StatusResponse {
	d := digest.FromString(fmt.Sprint(i)).String()
	at := timestamppb.New(t0.Add(time.Second + time.Duration(i)*time.Millisecond))
	msgs := []*controlapi.StatusResponse{{Vertexes: []*controlapi.Vertex{{Digest: d, Name: fmt.Sprintf("[app %d/9999] RUN loud", i+1), Started: at, Completed: at}}}}
	for len(log) > 0 {
		n := min(chunk, len(log))
		msgs = append(msgs, &controlapi.StatusResponse{Logs: []*controlapi.VertexLog{{Vertex: d, Stream: 1, Msg: []byte(log[:n])}}})
		log = log[n:]
	}
	return msgs
}

func readLoud(t *testing.T, msgs []*controlapi.StatusResponse) ([]*client.SolveStatus, *Build) {
	t.Helper()
	progress, err := DecodeProgress(bytes.NewReader(progressBlob(t, msgs)))
	if err != nil {
		t.Fatal(err)
	}
	rec := &controlapi.BuildHistoryRecord{Ref: "loud", CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute))}
	b, err := NewBuild(rec, progress, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return progress, b
}

// A long log is read keeping only its head and tail, and the step's output
// is what it would be from the whole log: secrets where the output is cut
// are masked whole, and the count of bytes cut is right.
func TestLongLogKeepsWhatTheOutputShows(t *testing.T) {
	token := func(c string) string { return " glpat-" + strings.Repeat(c, 20) }
	filler := func(n int) string { return strings.Repeat("log line\n", n/9+1)[:n] }
	head := "HEAD\n" + filler(outputHead-15)
	log := head + token("a") + filler(1<<20) + token("b") + filler(MaxStepOutput-outputHead-10) + "TAIL\n"
	if i := strings.Index(log, token("b")); len(log)-i <= MaxStepOutput-outputHead || len(log)-i-len(token("b")) >= MaxStepOutput-outputHead {
		t.Fatal("the second secret does not straddle where the output's tail starts")
	}
	_, b := readLoud(t, loudStep(0, log, 4<<10))
	if len(b.Steps) != 1 {
		t.Fatalf("%d steps", len(b.Steps))
	}
	if got, want := b.Steps[0].Output, clip(mask(log, nil)); got != want {
		t.Errorf("output differs from the whole log's\n got: %q … %q\nwant: %q … %q", got[outputHead-40:outputHead+40], got[len(got)-(MaxStepOutput-outputHead)-40:len(got)-(MaxStepOutput-outputHead)+40],
			want[outputHead-40:outputHead+40], want[len(want)-(MaxStepOutput-outputHead)-40:len(want)-(MaxStepOutput-outputHead)+40])
	}
	if strings.Contains(b.Steps[0].Output, "aaaa") || strings.Contains(b.Steps[0].Output, "bbbb") {
		t.Error("a secret where the output is cut is shown")
	}
}

// The logs kept of one blob are bounded however many steps printed: a log
// beyond the budget keeps no made-up tail, only the count of what it cut.
func TestLogsKeptOfOneBlobAreBounded(t *testing.T) {
	const steps = 450 // each 128 KiB: 56 MiB printed, more than the budget allows
	var msgs []*controlapi.StatusResponse
	for i := range steps {
		msgs = append(msgs, loudStep(i, strings.Repeat("x", 128<<10-len("END\n"))+"END\n", 64<<10)...)
	}
	progress, b := readLoud(t, msgs)
	var kept int
	for _, st := range progress {
		for _, l := range st.Logs {
			kept += len(l.Data)
		}
	}
	if kept > maxKeptLogs {
		t.Errorf("%d MiB of logs kept", kept>>20)
	}
	if len(b.Steps) != steps {
		t.Fatalf("%d steps", len(b.Steps))
	}
	first, last := b.Steps[0].Output, b.Steps[steps-1].Output
	if !strings.HasSuffix(first, "END\n") || !strings.Contains(first, "bytes cut") {
		t.Errorf("first step: %q", first[len(first)-40:])
	}
	if strings.HasSuffix(last, "END\n") || !strings.Contains(last, "bytes cut") {
		t.Errorf("last step: %q", last[max(0, len(last)-40):])
	}
}

// A log at the edge of the budget keeps a head cut at any byte, without
// the margin that lets masking see a secret whole: a secret cut there is
// not shown, neither the key it begins nor the part of a token before it.
func TestLogCutByTheBudgetShowsNoPartOfASecret(t *testing.T) {
	const kept = 1000 // what the budget leaves of the last log's head
	per := keptHead + 2*keptTail
	full := maxKeptLogs / per
	rest := maxKeptLogs - full*per
	if rest <= kept {
		t.Fatalf("budget left after %d logs: %d", full, rest)
	}
	pad := func(s string) string { return s + strings.Repeat("log line\n", kept/9+1)[:kept-len(s)] }
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEsecretkeybodyMIIEsecretkeybodyMIIEsecretkeybodyMIIEsecret1\n", 30) + "-----END RSA PRIVATE KEY-----\n"
	for name, c := range map[string]struct{ log, secret string }{
		// A key in a 64-character base64 line each, cut in its middle.
		"key": {log: pad("setup\n")[:kept-400] + key + "done\n", secret: "MIIEsecret"},
		// A token cut before its sixteenth character after the prefix.
		"token": {log: pad("setup\n")[:kept-10] + "glpat-abcdefghijklmnopqrstuv\ndone\n", secret: "glpat-abcd"},
	} {
		t.Run(name, func(t *testing.T) {
			var msgs []*controlapi.StatusResponse
			for i := range full {
				msgs = append(msgs, loudStep(i, strings.Repeat("x", keptHead+1), 64<<10)...)
			}
			msgs = append(msgs, loudStep(full, strings.Repeat("y\n", (rest-kept)/2), 64<<10)...)
			msgs = append(msgs, loudStep(full+1, c.log+strings.Repeat("after\n", 1000), 64<<10)...)
			_, b := readLoud(t, msgs)
			if len(b.Steps) != full+2 {
				t.Fatalf("%d steps", len(b.Steps))
			}
			out := b.Steps[full+1].Output
			if !strings.HasPrefix(out, "setup\n") || !strings.Contains(out, "bytes cut") {
				t.Fatalf("not the step cut by the budget: %q", out[:min(len(out), 80)])
			}
			if strings.Contains(out, c.secret) {
				t.Errorf("part of a secret shown: %q", out)
			}
		})
	}
}
