package history

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Def is one operation of a build's graph as the store keeps it: the
// normalised operation and the identities of its inputs. Identities make it
// content-addressed: builds that share a step share its Def.
type Def struct {
	Identity string
	Op       json.RawMessage
	Inputs   []string
}

// Defs returns every operation of the build's graph, steps and sources,
// ordered by identity. It is empty for a build without provenance.
func (b *Build) Defs() []Def {
	if b.prov == nil {
		return nil
	}
	out := make([]Def, 0, len(b.prov.Steps))
	for id, s := range b.prov.Steps {
		d := Def{Identity: b.prov.Identity[id], Op: s.Op, Inputs: make([]string, 0, len(s.Inputs))}
		for _, in := range s.Inputs {
			d.Inputs = append(d.Inputs, b.prov.Identity[in])
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

// Restore rebuilds a build from what the store keeps, so that FirstRebuild
// can compare it: its exported fields (steps with identities, daemon,
// build arguments, attribution) and the Defs of its graph. defs must hold
// every identity reachable from its steps.
func Restore(b Build, defs map[string]Def) (*Build, error) {
	p := &Provenance{
		Steps:    map[string]ProvStep{},
		Identity: map[string]string{},
		Lines:    map[string]int{},
		Ranges:   map[string][]LineRange{},
	}
	var add func(id string) error
	add = func(id string) error {
		if _, ok := p.Steps[id]; ok {
			return nil
		}
		d, ok := defs[id]
		if !ok {
			return fmt.Errorf("restore %s: no definition for %s", b.Ref, id)
		}
		p.Steps[id] = ProvStep{ID: id, Inputs: d.Inputs, Op: d.Op}
		p.Identity[id] = id
		for _, in := range d.Inputs {
			if err := add(in); err != nil {
				return err
			}
		}
		return nil
	}
	steps := make([]Step, len(b.Steps))
	for i, s := range b.Steps {
		if s.Identity != "" {
			if err := add(s.Identity); err != nil {
				return nil, err
			}
			s.StepID = s.Identity
			p.Lines[s.StepID] = s.Line
			p.Ranges[s.StepID] = s.Lines
		}
		steps[i] = s
	}
	b.Steps = steps
	b.prov = p
	return &b, nil
}

// maxAncestors bounds the operations StepOps walks: a graph is the
// client's to send.
const maxAncestors = 4096

// StepOps returns the normalised operation of a build's i-th step and the
// operations of every step it builds on (in no set order); nil when the
// build's graph is not known (no provenance, or not restored).
func (b *Build) StepOps(i int) (json.RawMessage, []json.RawMessage) {
	if b.prov == nil || i < 0 || i >= len(b.Steps) || b.Steps[i].StepID == "" {
		return nil, nil
	}
	s, ok := b.prov.Steps[b.Steps[i].StepID]
	if !ok {
		return nil, nil
	}
	var out []json.RawMessage
	seen := map[string]bool{s.ID: true}
	queue := append([]string(nil), s.Inputs...)
	for len(queue) > 0 && len(seen) < maxAncestors {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		a, ok := b.prov.Steps[id]
		if !ok {
			continue
		}
		out = append(out, a.Op)
		queue = append(queue, a.Inputs...)
	}
	return s.Op, out
}

// LastRun reads a provenance and returns the operation of the step made
// by the Dockerfile's last line, the operations it builds on, and the
// step's instruction as written ("RUN ..."), for tools that check hints
// against real records.
func LastRun(data []byte) (json.RawMessage, []json.RawMessage, string, error) {
	p, err := ParseProvenance(data, []byte("test"))
	if err != nil {
		return nil, nil, "", err
	}
	lines := strings.Split(strings.TrimRight(p.Dockerfile, "\n"), "\n")
	last := len(lines)
	for id, rs := range p.Ranges {
		for _, r := range rs {
			if r.End == last {
				b := &Build{prov: p, Steps: []Step{{StepID: id}}}
				op, anc := b.StepOps(0)
				return op, anc, strings.Join(lines[r.Start-1:r.End], "\n"), nil
			}
		}
	}
	return nil, nil, "", fmt.Errorf("no step for line %d", last)
}
