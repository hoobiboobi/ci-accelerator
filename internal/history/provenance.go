package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Provenance is the part of a record's SLSA v1 provenance the console uses.
type Provenance struct {
	// Steps by id ("step0", ...), each with its LLB operation.
	Steps map[string]ProvStep
	// DigestMapping joins progress vertices to steps.
	DigestMapping map[string]string
	// Lines maps a step id to its first Dockerfile line.
	Lines map[string]int
	// Args are the build request's attributes (build-arg:*, target, ...).
	Args map[string]string
	// Identity of every step: stable across builds (see identity).
	Identity map[string]string
}

// ProvStep is one LLB operation of the build definition.
type ProvStep struct {
	ID     string
	Inputs []string // step ids
	Op     json.RawMessage
}

type rawProvenance struct {
	BuildDefinition struct {
		ExternalParameters struct {
			Request struct {
				Args map[string]string `json:"args"`
			} `json:"request"`
		} `json:"externalParameters"`
		InternalParameters struct {
			BuildConfig struct {
				LLBDefinition []struct {
					ID     string          `json:"id"`
					Op     json.RawMessage `json:"op"`
					Inputs []string        `json:"inputs"`
				} `json:"llbDefinition"`
				DigestMapping map[string]string `json:"digestMapping"`
			} `json:"buildConfig"`
		} `json:"internalParameters"`
	} `json:"buildDefinition"`
	RunDetails struct {
		Metadata struct {
			BuildKit struct {
				Source struct {
					Locations map[string]struct {
						Locations []struct {
							Ranges []struct {
								Start struct {
									Line int `json:"line"`
								} `json:"start"`
							} `json:"ranges"`
						} `json:"locations"`
					} `json:"locations"`
				} `json:"source"`
			} `json:"buildkit_metadata"`
		} `json:"metadata"`
	} `json:"runDetails"`
}

// ParseProvenance reads a record's provenance attestation.
func ParseProvenance(data []byte) (*Provenance, error) {
	var raw rawProvenance
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("provenance: %w", err)
	}
	bc := raw.BuildDefinition.InternalParameters.BuildConfig
	p := &Provenance{
		Steps:         make(map[string]ProvStep, len(bc.LLBDefinition)),
		DigestMapping: bc.DigestMapping,
		Lines:         map[string]int{},
		Args:          raw.BuildDefinition.ExternalParameters.Request.Args,
		Identity:      map[string]string{},
	}
	for _, s := range bc.LLBDefinition {
		inputs := make([]string, 0, len(s.Inputs))
		for _, in := range s.Inputs {
			id, _, _ := strings.Cut(in, ":") // "step3:0" -> "step3"
			inputs = append(inputs, id)
		}
		p.Steps[s.ID] = ProvStep{ID: s.ID, Inputs: inputs, Op: s.Op}
	}
	for id, l := range raw.RunDetails.Metadata.BuildKit.Source.Locations {
		if len(l.Locations) > 0 && len(l.Locations[0].Ranges) > 0 {
			p.Lines[id] = l.Locations[0].Ranges[0].Start.Line
		}
	}
	for id := range p.Steps {
		if _, err := p.identity(id, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// identity is a hash of the step's operation and of its inputs' identities.
// BuildKit's own vertex digests change in every build for each step that
// depends on the build context (the local source carries session data), so
// steps of different builds are compared by this instead. A local source is
// reduced to its name: the paths a target needs differ between targets of
// one Dockerfile, but that is not what a step reads.
func (p *Provenance) identity(id string, visiting map[string]bool) (string, error) {
	if v, ok := p.Identity[id]; ok {
		return v, nil
	}
	if visiting[id] {
		return "", fmt.Errorf("provenance: cycle at %s", id)
	}
	visiting[id] = true
	s, ok := p.Steps[id]
	if !ok {
		return "", fmt.Errorf("provenance: unknown input %s", id)
	}
	h := sha256.New()
	h.Write(normalizeOp(s.Op))
	for _, in := range s.Inputs {
		v, err := p.identity(in, visiting)
		if err != nil {
			return "", err
		}
		h.Write([]byte("|" + v))
	}
	v := hex.EncodeToString(h.Sum(nil))[:16]
	p.Identity[id] = v
	return v, nil
}

// normalizeOp returns the op as canonical JSON, without a local source's
// attributes.
func normalizeOp(op json.RawMessage) []byte {
	var v map[string]any
	if err := json.Unmarshal(op, &v); err != nil {
		return op
	}
	if inner, ok := v["Op"].(map[string]any); ok {
		if src, ok := inner["source"].(map[string]any); ok {
			if id, _ := src["identifier"].(string); strings.HasPrefix(id, "local://") {
				delete(src, "attrs")
			}
		}
	}
	out, err := json.Marshal(v) // map keys are sorted: canonical
	if err != nil {
		return op
	}
	return out
}

// opView is the part of an LLB operation the reasons look at.
type opView struct {
	Op struct {
		Source *struct {
			Identifier string `json:"identifier"`
		} `json:"source"`
		Exec *struct {
			Meta struct {
				Args []string `json:"args"`
				Env  []string `json:"env"`
			} `json:"meta"`
		} `json:"exec"`
		File *struct {
			Actions []struct {
				Action struct {
					Copy *struct {
						Src  string `json:"src"`
						Dest string `json:"dest"`
					} `json:"copy"`
				} `json:"Action"`
			} `json:"actions"`
		} `json:"file"`
	} `json:"Op"`
}

func viewOf(op json.RawMessage) opView {
	var v opView
	_ = json.Unmarshal(op, &v)
	return v
}

// localInput reports whether a step reads a local source directly.
func (p *Provenance) localInput(id string) bool {
	for _, in := range p.Steps[id].Inputs {
		if src := viewOf(p.Steps[in].Op).Op.Source; src != nil && strings.HasPrefix(src.Identifier, "local://") {
			return true
		}
	}
	return false
}

// copySources lists the source paths of a step's copy actions.
func (p *Provenance) copySources(id string) []string {
	var out []string
	if f := viewOf(p.Steps[id].Op).Op.File; f != nil {
		for _, a := range f.Actions {
			if c := a.Action.Copy; c != nil {
				out = append(out, strings.TrimPrefix(c.Src, "/"))
			}
		}
	}
	sort.Strings(out)
	return out
}

// imageSource returns a step's image reference, if it is an image source.
func (p *Provenance) imageSource(id string) string {
	if src := viewOf(p.Steps[id].Op).Op.Source; src != nil && strings.HasPrefix(src.Identifier, "docker-image://") {
		return strings.TrimPrefix(src.Identifier, "docker-image://")
	}
	return ""
}

// env returns an exec step's environment as a map.
func (p *Provenance) env(id string) map[string]string {
	out := map[string]string{}
	if e := viewOf(p.Steps[id].Op).Op.Exec; e != nil {
		for _, kv := range e.Meta.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
	}
	return out
}
