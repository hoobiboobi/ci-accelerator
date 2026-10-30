package history

import (
	"encoding/json"
	"strings"
	"testing"
)

// A step's output is kept where the build executed it, and only there.
func TestStepOutputWhereExecuted(t *testing.T) {
	bs := loadFixtures(t)
	if s := step(t, byJob(t, bs, 1001, "api"), "[deps 6/6]"); s.Output != "deps installed\n" {
		t.Errorf("built step: output %q", s.Output)
	}
	if s := step(t, byJob(t, bs, 1002, "api"), "[deps 6/6]"); s.State != Cached || s.Output != "" {
		t.Errorf("cached step: %s, output %q", s.State, s.Output)
	}
	broken := step(t, byJob(t, bs, 1010, "broken"), "[broken 1/1]")
	if broken.Output != "failing step\n" || !strings.Contains(broken.Error, "exit code: 3") {
		t.Errorf("failed step: output %q, error %q", broken.Output, broken.Error)
	}
	for _, b := range bs {
		for _, s := range b.Steps {
			if s.Output != "" && s.State != Built {
				t.Errorf("job %d %s: %s step with output %q", b.Attribution.Job, s.Key, s.State, s.Output)
			}
		}
	}
}

func TestMask(t *testing.T) {
	args := map[string]string{"NPM_TOKEN": "npm-s3cr3t-value", "DB_PASSWORD": "hunter22", "VERSION": "1.2.3", "API_KEY": "abc"}
	for in, want := range map[string]string{
		"token glpat-AbCdEfGhIjKlMnOpQrSt done":                                                "token [masked] done",
		"runner glrt-t3_AbCdEfGhIjKlMnOpQrStUv ok":                                             "runner [masked] ok",
		"job glcbt-64_AbCdEfGhIjKlMnOpQr":                                                      "job [masked]",
		"gh ghp_" + strings.Repeat("a1", 18) + " end":                                          "gh [masked] end",
		"github_pat_" + strings.Repeat("Ab1_", 6):                                              "[masked]",
		"aws AKIAABCDEFGHIJKLMNOP":                                                             "aws [masked]",
		"slack xoxb-1234567890-abcdef":                                                         "slack [masked]",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----\nnext": "[masked]\nnext",
		"git clone https://oauth2:p4ss@gitlab.example.com/a/b.git":                             "git clone https://oauth2:[masked]@gitlab.example.com/a/b.git",
		"Authorization: Bearer abc.def.ghi":                                                    "Authorization: Bearer [masked]",
		"Cookie: _gitlab_session=0123abcd":                                                     "Cookie: [masked]",
		"npm config set //registry/:_authToken=npm-s3cr3t-value":                               "npm config set //registry/:_authToken=[masked]",
		"connecting with hunter22":                                                             "connecting with [masked]",
		// Right after a letter, as in a printf's literal \n.
		`printf 'glpat-AbCdEfGhIjKlMnOpQrSt\nghp_` + strings.Repeat("a1", 18) + `\nAKIAABCDEFGHIJKLMNOP\n'`:  `printf '[masked]\n[masked]\n[masked]\n'`,
		`echo x\ngithub_pat_` + strings.Repeat("Ab1_", 6) + `\nxoxb-1234567890-abcdef\nASIAABCDEFGHIJKLMNOP`: `echo x\n[masked]\n[masked]\n[masked]`,
		"key=AKIAABCDEFGHIJKLMNOP": "key=[masked]",
		// Not secrets: short or non-secret build arguments, look-alikes.
		"version 1.2.3 with abc":                  "version 1.2.3 with abc",
		"glpat is a prefix, glpat-short is not":   "glpat is a prefix, glpat-short is not",
		"see https://gitlab.example.com/a/b.git":  "see https://gitlab.example.com/a/b.git",
		"AKIA alone, ghp_short, xoxb- and tokens": "AKIA alone, ghp_short, xoxb- and tokens",
		// AKIA inside a longer run of capitals and digits is not a key id.
		"HASH0AKIAABCDEFGHIJKLMNOP and AKIAABCDEFGHIJKLMNOPQ": "HASH0AKIAABCDEFGHIJKLMNOP and AKIAABCDEFGHIJKLMNOPQ",
	} {
		if got := mask(in, args); got != want {
			t.Errorf("mask(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// A job's log is read in parts, so a printed private key can be cut in
// two: each half is masked on its own, the rest of the part kept.
func TestMaskSecretsHalfAKey(t *testing.T) {
	for in, want := range map[string]string{
		"before\n-----BEGIN RSA PRIVATE KEY-----\nBODYLINE1\nBODYLINE2\n":                                          "before\n[masked]",
		"BODYLINE1\nBODYLINE2\n-----END RSA PRIVATE KEY-----\nafter\n":                                             "[masked]\nafter\n",
		"BODY1\n-----END EC PRIVATE KEY-----\nmid\n-----BEGIN EC PRIVATE KEY-----\nBODY2\n":                        "[masked]\nmid\n[masked]",
		"BODY1\n-----END EC PRIVATE KEY-----\n-----BEGIN EC PRIVATE KEY-----\nB\n-----END EC PRIVATE KEY-----\nok": "[masked]\n[masked]\nok",
		"no key here\n-----BEGIN CERTIFICATE-----\nMIIB\n":                                                         "no key here\n-----BEGIN CERTIFICATE-----\nMIIB\n",
	} {
		if got := MaskSecrets(in); got != want {
			t.Errorf("MaskSecrets(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// A Dockerfile is shown by line number: masking keeps its lines.
func TestMaskDockerfile(t *testing.T) {
	in := "FROM alpine\nRUN printf 'x\\nghp_" + strings.Repeat("a1", 18) + "'\nCOPY <<EOF /k\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\n-----END OPENSSH PRIVATE KEY-----\nEOF\nRUN true\n"
	want := "FROM alpine\nRUN printf 'x\\n[masked]'\nCOPY <<EOF /k\n[masked]\n\n\nEOF\nRUN true\n"
	if got := MaskDockerfile(in); got != want {
		t.Errorf("MaskDockerfile\n got %q\nwant %q", got, want)
	}
	// A build's Dockerfile, as stored, also hides its secret build
	// arguments' values.
	b := &Build{prov: &Provenance{DockerfileName: "Dockerfile", Dockerfile: in + "ARG NPM_TOKEN=npm-s3cr3t-value\n"},
		BuildArgs: map[string]string{"NPM_TOKEN": "npm-s3cr3t-value"}}
	if _, got := b.Dockerfile(); got != want+"ARG NPM_TOKEN=[masked]\n" {
		t.Errorf("Dockerfile()\n got %q\nwant %q", got, want+"ARG NPM_TOKEN=[masked]\n")
	}
}

func TestClip(t *testing.T) {
	if s := strings.Repeat("x", MaxStepOutput); clip(s) != s {
		t.Error("output at the limit was clipped")
	}
	long := "HEAD" + strings.Repeat("a", 200<<10) + "TAIL"
	got := clip(long)
	if len(got) > MaxStepOutput+64 || !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "bytes cut") {
		t.Errorf("clipped to %d bytes: %q … %q", len(got), got[:10], got[len(got)-10:])
	}
}

// A build argument whose name says it is a secret never shows its values,
// not even in the reason for a rebuild.
func TestSecretBuildArgumentInAReason(t *testing.T) {
	bs := loadFixtures(t)
	prev := renameArg(t, byJob(t, bs, 1001, "api"), "VERSION", "API_TOKEN")
	cur := renameArg(t, byJob(t, bs, 1002, "api"), "VERSION", "API_TOKEN")
	r, ok := FirstRebuild(prev, cur)
	if !ok || r.Kind != ArgChanged || r.Arg != "API_TOKEN" || r.Old != Masked || r.New != Masked {
		t.Errorf("%+v", r)
	}
}

// renameArg copies a build with a build argument renamed in its request
// and in the environment of its steps.
func renameArg(t *testing.T, b *Build, from, to string) *Build {
	t.Helper()
	c := *b
	c.BuildArgs = map[string]string{}
	for k, v := range b.BuildArgs {
		if k == from {
			k = to
		}
		c.BuildArgs[k] = v
	}
	p := *b.prov
	p.Steps = map[string]ProvStep{}
	for id, s := range b.prov.Steps {
		op := strings.ReplaceAll(string(s.Op), `"`+from+`=`, `"`+to+`=`)
		if !json.Valid([]byte(op)) {
			t.Fatalf("%s: op no longer JSON", id)
		}
		s.Op = json.RawMessage(op)
		p.Steps[id] = s
	}
	c.prov = &p
	return &c
}

// A secret build argument's value never stays in a kept operation, yet a
// changed secret still changes the step's identity.
func TestSecretValuesInOperations(t *testing.T) {
	prov := func(token string) []byte {
		return []byte(`{"buildDefinition":{"internalParameters":{"buildConfig":{"llbDefinition":[
			{"id":"step0","op":{"Op":{"exec":{"meta":{"args":["sh","-c","make"],"env":["API_TOKEN=` + token + `","VERSION=1"]}}}}}]}}}}`)
	}
	parse := func(token, key string) *Provenance {
		t.Helper()
		p, err := ParseProvenance(prov(token), []byte(key))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, again, changed, otherKey := parse("s3cr3t-value", "k1"), parse("s3cr3t-value", "k1"), parse("n3w-value", "k1"), parse("s3cr3t-value", "k2")
	op := string(a.Steps["step0"].Op)
	if strings.Contains(op, "s3cr3t") || !strings.Contains(op, "API_TOKEN=[masked:") || !strings.Contains(op, "VERSION=1") {
		t.Errorf("kept operation: %s", op)
	}
	if a.Identity["step0"] != again.Identity["step0"] {
		t.Error("the same secret gave different identities")
	}
	if a.Identity["step0"] == changed.Identity["step0"] {
		t.Error("a changed secret kept the identity")
	}
	if string(a.Steps["step0"].Op) == string(otherKey.Steps["step0"].Op) {
		t.Error("the mask does not depend on the key")
	}
	if got := string(normalizeOp(a.Steps["step0"].Op, []byte("k1"))); got != op {
		t.Errorf("normalising twice changed the operation:\n%s\n%s", op, got)
	}
}

// Several build arguments changed at once: still not a Dockerfile change.
func TestSeveralBuildArgumentsChanged(t *testing.T) {
	bs := loadFixtures(t)
	prev := byJob(t, bs, 1001, "api")
	cur := addArg(t, byJob(t, bs, 1002, "api"), "EXTRA", "x")
	r, ok := FirstRebuild(prev, cur)
	if !ok || r.Kind != ArgChanged || r.Arg != "EXTRA, VERSION" || r.Old != "" || r.New != "" {
		t.Errorf("%+v", r)
	}
}

// addArg copies a build with one more build argument, set in the
// environment of every step that has VERSION.
func addArg(t *testing.T, b *Build, name, value string) *Build {
	t.Helper()
	c := *b
	c.BuildArgs = map[string]string{name: value}
	for k, v := range b.BuildArgs {
		c.BuildArgs[k] = v
	}
	p := *b.prov
	p.Steps = map[string]ProvStep{}
	for id, s := range b.prov.Steps {
		op := strings.ReplaceAll(string(s.Op), `"VERSION=`, `"`+name+`=`+value+`","VERSION=`)
		if !json.Valid([]byte(op)) {
			t.Fatalf("%s: op no longer JSON", id)
		}
		s.Op = json.RawMessage(op)
		p.Steps[id] = s
	}
	c.prov = &p
	return &c
}
