package history

import (
	"encoding/json"
	"fmt"
	"sort"
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
