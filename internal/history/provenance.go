package history

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
)

// Provenance is the part of a record's SLSA v1 provenance the console uses.
type Provenance struct {
	// Steps by id ("step0", ...), each with its LLB operation.
	Steps map[string]ProvStep
	// DigestMapping joins progress vertices to steps.
	DigestMapping map[string]string
	// Lines maps a step id to its first Dockerfile line.
	Lines map[string]int
	// Ranges maps a step id to the Dockerfile lines it came from: a RUN
	// continued over several lines is one range, a step built from several
	// instructions has several.
	Ranges map[string][]LineRange
	// Dockerfile is the text of the Dockerfile the build used and
	// DockerfileName its file name, as the frontend recorded them.
	Dockerfile     string
	DockerfileName string
	// Args are the build request's attributes (build-arg:*, target, ...).
	Args map[string]string
	// Identity of every step: stable across builds (see identity).
	Identity map[string]string
	// Usage is what each executed step's processes used, by step id.
	Usage map[string]Usage
}

// Usage is what a step's processes used, from the cgroup counters BuildKit
// samples while it runs and once when it ends: totals, not estimates.
type Usage struct {
	CPU        time.Duration // user + system time of every process
	Throttled  time.Duration // time the CPU limit held them back
	ReadBytes  int64         // read from block devices
	WriteBytes int64         // written to block devices
	// Memory is the most its cgroup held in one of BuildKit's samples
	// (anonymous memory, file cache and kernel memory: what a memory limit
	// counts), not a true peak: a short spike between samples is missed.
	Memory int64
}

// LineRange is a range of Dockerfile lines, both ends included.
type LineRange struct {
	Start, End int
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
					ID            string          `json:"id"`
					Op            json.RawMessage `json:"op"`
					Inputs        []string        `json:"inputs"`
					ResourceUsage *struct {
						Samples []struct {
							CPUStat *struct {
								UsageNanos     int64 `json:"usageNanos"`
								ThrottledNanos int64 `json:"throttledNanos"`
							} `json:"cpuStat"`
							IOStat *struct {
								ReadBytes  int64 `json:"readBytes"`
								WriteBytes int64 `json:"writeBytes"`
							} `json:"ioStat"`
							MemoryStat *struct {
								Anon        int64 `json:"anon"`
								File        int64 `json:"file"`
								KernelStack int64 `json:"kernelStack"`
								PageTables  int64 `json:"pageTables"`
								Slab        int64 `json:"slab"`
								Sock        int64 `json:"sock"`
							} `json:"memoryStat"`
						} `json:"samples"`
					} `json:"resourceUsage"`
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
							SourceIndex int `json:"sourceIndex"`
							Ranges      []struct {
								Start struct {
									Line int `json:"line"`
								} `json:"start"`
								End struct {
									Line int `json:"line"`
								} `json:"end"`
							} `json:"ranges"`
						} `json:"locations"`
					} `json:"locations"`
					Infos []struct {
						Filename string `json:"filename"`
						Language string `json:"language"`
						Data     []byte `json:"data"`
					} `json:"infos"`
				} `json:"source"`
			} `json:"buildkit_metadata"`
		} `json:"metadata"`
	} `json:"runDetails"`
}

// ParseProvenance reads a record's provenance attestation. Each step's
// operation is kept normalised (see normalizeOp); key is the
// installation's key for hashing secret values.
func ParseProvenance(data []byte, key []byte) (*Provenance, error) {
	var raw rawProvenance
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("provenance: %w", err)
	}
	bc := raw.BuildDefinition.InternalParameters.BuildConfig
	p := &Provenance{
		Steps:         make(map[string]ProvStep, len(bc.LLBDefinition)),
		DigestMapping: bc.DigestMapping,
		Lines:         map[string]int{},
		Ranges:        map[string][]LineRange{},
		Args:          raw.BuildDefinition.ExternalParameters.Request.Args,
		Identity:      map[string]string{},
		Usage:         map[string]Usage{},
	}
	for _, s := range bc.LLBDefinition {
		inputs := make([]string, 0, len(s.Inputs))
		for _, in := range s.Inputs {
			id, _, _ := strings.Cut(in, ":") // "step3:0" -> "step3"
			inputs = append(inputs, id)
		}
		p.Steps[s.ID] = ProvStep{ID: s.ID, Inputs: inputs, Op: normalizeOp(s.Op, key)}
		if ru := s.ResourceUsage; ru != nil && len(ru.Samples) > 0 {
			// The counters only grow: the largest sample is the total.
			var u Usage
			for _, x := range ru.Samples {
				if c := x.CPUStat; c != nil {
					u.CPU = max(u.CPU, time.Duration(c.UsageNanos))
					u.Throttled = max(u.Throttled, time.Duration(c.ThrottledNanos))
				}
				if io := x.IOStat; io != nil {
					u.ReadBytes, u.WriteBytes = max(u.ReadBytes, io.ReadBytes), max(u.WriteBytes, io.WriteBytes)
				}
				if m := x.MemoryStat; m != nil {
					// Memory is not a counter: the largest sample is kept.
					u.Memory = max(u.Memory, m.Anon+m.File+m.KernelStack+m.PageTables+m.Slab+m.Sock)
				}
			}
			p.Usage[s.ID] = u
		}
	}
	src := raw.RunDetails.Metadata.BuildKit.Source
	dockerfile := 0 // the first source, unless one is marked as a Dockerfile
	for i, in := range src.Infos {
		if in.Language == "Dockerfile" {
			dockerfile = i
			break
		}
	}
	if dockerfile < len(src.Infos) {
		p.Dockerfile = string(src.Infos[dockerfile].Data)
		p.DockerfileName = src.Infos[dockerfile].Filename
	}
	for id, l := range src.Locations {
		for _, loc := range l.Locations {
			if loc.SourceIndex != dockerfile {
				continue
			}
			for _, r := range loc.Ranges {
				end := r.End.Line
				if end < r.Start.Line {
					end = r.Start.Line
				}
				p.Ranges[id] = append(p.Ranges[id], LineRange{Start: r.Start.Line, End: end})
			}
		}
		if rs := p.Ranges[id]; len(rs) > 0 {
			sort.Slice(rs, func(i, j int) bool { return rs[i].Start < rs[j].Start })
			p.Lines[id] = rs[0].Start
		}
	}
	for id := range p.Steps {
		if _, err := p.identity(id, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// mergeProvenance joins the provenances of a build's results into one. A
// build for several platforms has one per platform, each numbering its
// steps from step0 and mapping only that platform's vertices, so the steps
// of every platform after the first get their own ids ("p1.step3"). A step
// with the same identity as one of an earlier platform (a stage built for
// the build platform, the build context) is the same operation, with the
// same vertex: it keeps the earlier id. Dockerfile and request come from
// the first.
func mergeProvenance(ps []*Provenance) *Provenance {
	if len(ps) == 1 {
		return ps[0]
	}
	m := *ps[0]
	m.Steps, m.DigestMapping, m.Identity = maps.Clone(m.Steps), maps.Clone(m.DigestMapping), maps.Clone(m.Identity)
	m.Lines, m.Ranges, m.Usage = maps.Clone(m.Lines), maps.Clone(m.Ranges), maps.Clone(m.Usage)
	byIdentity := map[string]string{}
	for id, v := range m.Identity {
		byIdentity[v] = id
	}
	for i, p := range ps[1:] {
		ids := make(map[string]string, len(p.Steps))
		for id := range p.Steps {
			if same, ok := byIdentity[p.Identity[id]]; ok {
				ids[id] = same
			} else {
				ids[id] = fmt.Sprintf("p%d.%s", i+1, id)
			}
		}
		for id, s := range p.Steps {
			to := ids[id]
			if _, ok := m.Steps[to]; ok {
				continue
			}
			inputs := make([]string, len(s.Inputs))
			for j, in := range s.Inputs {
				inputs[j] = ids[in]
			}
			m.Steps[to] = ProvStep{ID: to, Inputs: inputs, Op: s.Op}
			m.Identity[to] = p.Identity[id]
			byIdentity[p.Identity[id]] = to
			if l, ok := p.Lines[id]; ok {
				m.Lines[to] = l
			}
			if r, ok := p.Ranges[id]; ok {
				m.Ranges[to] = r
			}
			if u, ok := p.Usage[id]; ok {
				m.Usage[to] = u
			}
		}
		for d, id := range p.DigestMapping {
			if _, ok := m.DigestMapping[d]; !ok {
				m.DigestMapping[d] = ids[id]
			}
		}
	}
	return &m
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
	h.Write(s.Op)
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
// attributes, and with the value of every environment variable whose name
// says it is a secret replaced by a keyed hash: a changed secret still
// changes the operation, but the value is not kept. It is idempotent.
func normalizeOp(op json.RawMessage, key []byte) json.RawMessage {
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
		if exec, ok := inner["exec"].(map[string]any); ok {
			if meta, ok := exec["meta"].(map[string]any); ok {
				if env, ok := meta["env"].([]any); ok {
					for i, e := range env {
						s, _ := e.(string)
						if k, v, ok := strings.Cut(s, "="); ok && SecretArg(k) && !strings.HasPrefix(v, keyedPrefix) {
							env[i] = k + "=" + keyedMask(key, v)
						}
					}
				}
			}
		}
	}
	out, err := json.Marshal(v) // map keys are sorted: canonical
	if err != nil {
		return op
	}
	return out
}

const keyedPrefix = "[masked:"

// keyedMask hides a secret value behind a keyed hash: equal values give
// equal masks, so a change is still visible.
func keyedMask(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return keyedPrefix + hex.EncodeToString(m.Sum(nil))[:16] + "]"
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

// contextDependent reports whether a step's cache key depends on the build
// context: a local source anywhere among its transitive inputs. The step
// identity strips local-source content on purpose (see identity), so a step
// can be rebuilt with an unchanged identity whenever context files fed any
// ancestor — not only when it reads them itself (localInput).
func (p *Provenance) contextDependent(id string) bool {
	return p.walkCone(id, map[string]bool{}, nil)
}

// contextSources lists what the build context feeds into a step's ancestry:
// the copy sources of every step in its cone that reads a local source
// directly, and "." for one that reads it without copy actions. Broad on
// purpose: matching a changed file against these errs towards "your
// change", never away from it.
func (p *Provenance) contextSources(id string) []string {
	set := map[string]bool{}
	p.walkCone(id, map[string]bool{}, set)
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// walkCone visits a step and its transitive inputs. With sources nil it
// reports whether the cone contains a local source; otherwise it collects
// every direct reader's copy sources into sources and walks the whole cone.
func (p *Provenance) walkCone(id string, seen map[string]bool, sources map[string]bool) bool {
	if seen[id] {
		return false
	}
	seen[id] = true
	s, ok := p.Steps[id]
	if !ok {
		return false
	}
	if src := viewOf(s.Op).Op.Source; src != nil && strings.HasPrefix(src.Identifier, "local://") {
		return true
	}
	if sources != nil && p.localInput(id) {
		cs := p.copySources(id)
		if len(cs) == 0 {
			cs = []string{"."}
		}
		for _, c := range cs {
			sources[c] = true
		}
	}
	found := false
	for _, in := range s.Inputs {
		if p.walkCone(in, seen, sources) {
			if sources == nil {
				return true
			}
			found = true
		}
	}
	return found
}

// copySources lists the source paths of a step's copy actions.
func (p *Provenance) copySources(id string) []string {
	var out []string
	if f := viewOf(p.Steps[id].Op).Op.File; f != nil {
		for _, a := range f.Actions {
			if c := a.Action.Copy; c != nil {
				src := strings.TrimPrefix(c.Src, "/")
				if src == "" {
					src = "." // the whole context
				}
				out = append(out, src)
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
