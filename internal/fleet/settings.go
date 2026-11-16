package fleet

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A cluster's settings at the hub: what "Add a cluster" asks for, from a
// curated list of parameters (ParamsVersion). The hub turns them into the
// box's values and install commands (installing stays Helm), and keeps
// the secret ones (tokens, keys) sealed with a key of its own, to deliver
// them to the cluster over its link: written there into Secrets that the
// generated values name. Secrets are write-only: no page and no answer of
// the API shows one again.
//
// Another parameter is one entry of Params and its check; generated values
// follow from its Path.

// ParamsVersion is the version of the parameter list, kept with each
// cluster's settings.
const ParamsVersion = 1

// Param kinds.
const (
	KindText   = "text"
	KindNumber = "number"
	KindChoice = "choice"
	KindSecret = "secret"
)

// Param is one setting of a cluster.
type Param struct {
	Key, Section, Label, Kind, Help string
	// Choices of a KindChoice param: value -> label, in order.
	Choices [][2]string
	Default string
	// Required: the form refuses an empty value (when When holds).
	Required bool
	// When: the param applies only when another param has this value
	// ("storage=hostPath"); "" always.
	When string
	// Path: where its value goes in the box's values ("a.b.c"); "" for a
	// param the generation handles itself.
	Path string
	// Secret: the Secret (and its key) a KindSecret param is delivered in.
	Secret, SecretKey string
	check             func(string) error
}

// Secrets the hub delivers to a member: the names the generated values
// refer to and the member may write (and nothing else).
const (
	SecretRunner = "fleet-runner-token"
	SecretGitLab = "fleet-gitlab-token"
	SecretS3     = "fleet-s3-cache"
)

// DeliveredSecrets are the names a member writes when the hub delivers
// them.
var DeliveredSecrets = []string{SecretRunner, SecretGitLab, SecretS3}

var (
	sizeRe = regexp.MustCompile(`^([1-9][0-9]{0,5}(GB|TB)|[1-9][0-9]?%)$`)
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// printable says whether v is printable ASCII only: a YAML reader keeps
// it as it is (it folds some other characters, U+0085 among them).
func printable(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

func httpsURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || !printable(v) || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return errors.New("an http(s) URL without a user or password, like https://gitlab.example.com/")
	}
	return nil
}

// absPath: an absolute path without empty, "." or ".." parts.
func absPath(v string) error {
	ok := pathRe.MatchString(v)
	for _, part := range strings.Split(strings.TrimPrefix(v, "/"), "/") {
		ok = ok && part != "" && part != "." && part != ".."
	}
	if !ok {
		return errors.New("an absolute path, like /var/mnt/nvme/buildkit")
	}
	return nil
}

func numberIn(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("a number from %d to %d", lo, hi)
		}
		return nil
	}
}

func matches(re *regexp.Regexp, what string) func(string) error {
	return func(v string) error {
		if !re.MatchString(v) {
			return errors.New(what)
		}
		return nil
	}
}

// Params is the curated list, in the form's order.
var Params = []Param{
	{Key: "joins", Section: "Cluster", Label: "Joins with", Kind: KindChoice, Default: "token",
		Choices: [][2]string{{"token", "Join token"}, {"key", "Public key"}}},
	{Key: "token_hours", Section: "Cluster", Label: "Token valid for", Kind: KindChoice, Default: "1", When: "joins=token",
		Choices: [][2]string{{"1", "1 hour"}, {"24", "1 day"}, {"168", "7 days"}}, Help: "The token works once."},
	{Key: "public_key", Section: "Cluster", Label: "Public key", Kind: KindText, When: "joins=key", Required: true,
		Help: "PEM of the key the box will use (ci-accelerator fleet-keygen makes a pair).",
		check: func(v string) error {
			ks, err := ParsePublicKeys([]byte(v))
			if err != nil || len(ks) != 1 {
				return errors.New("one public key, PEM (-----BEGIN PUBLIC KEY-----)")
			}
			return nil
		}},

	{Key: "storage", Section: "Build cache disk", Label: "Storage", Kind: KindChoice, Default: "hostPath", Path: "buildkit.storage.type",
		Choices: [][2]string{{"hostPath", "A disk on the build nodes"}, {"emptyDir", "The node's root disk, lost on restart"}}},
	{Key: "host_path", Section: "Build cache disk", Label: "Path on build nodes", Kind: KindText, Default: "/var/lib/buildkit", When: "storage=hostPath",
		Required: true, Path: "buildkit.storage.hostPath.path", Help: "A directory on the cache disk, made in advance.",
		check: absPath},
	{Key: "cache_limit", Section: "Build cache disk", Label: "Cache limit", Kind: KindText, Default: "75%", Path: "buildkit.config.gc.maxUsedSpace",
		Help:  "BuildKit's units: 400GB, 2TB, or a share of the disk, 75%.",
		check: matches(sizeRe, "like 400GB, 2TB or 75%")},

	{Key: "gitlab_url", Section: "Runner", Label: "GitLab", Kind: KindText, Required: true, Path: "gitlab-runner.gitlabUrl",
		Help: "Exactly as the runner registers, slash included.", check: httpsURL},
	{Key: "runner_token", Section: "Runner", Label: "Runner token", Kind: KindSecret, Required: true,
		Secret: SecretRunner, SecretKey: "runner-token", Help: "glrt-…: sent to the cluster, never shown again.",
		check: func(v string) error {
			if !strings.HasPrefix(v, "glrt-") || len(v) < 20 {
				return errors.New("a runner authentication token, glrt-…")
			}
			return nil
		}},
	{Key: "concurrent", Section: "Runner", Label: "Concurrent jobs", Kind: KindNumber, Default: "10", Path: "gitlab-runner.concurrent",
		check: numberIn(1, 1000)},
	{Key: "gitlab_token", Section: "Runner", Label: "GitLab read token", Kind: KindSecret, Secret: SecretGitLab, SecretKey: "token",
		Help: "Optional, read_api: job times and the end of job logs on the pages."},

	{Key: "s3_server", Section: "S3 cache for the runner", Label: "Server", Kind: KindText, Path: "gitlab-runner.runners.cache.s3.server",
		Help: "Optional: host[:port], like s3.eu-central-1.amazonaws.com.", check: matches(hostRe, "host or host:port, no https://")},
	{Key: "s3_bucket", Section: "S3 cache for the runner", Label: "Bucket", Kind: KindText, Path: "gitlab-runner.runners.cache.s3.bucket", When: "s3_server=*", Required: true,
		check: matches(regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`), "an S3 bucket name")},
	{Key: "s3_region", Section: "S3 cache for the runner", Label: "Region", Kind: KindText, Path: "gitlab-runner.runners.cache.s3.region", When: "s3_server=*",
		Required: true, Help: "The box's chart needs one with a server; any value for a server without regions.",
		check: matches(regexp.MustCompile(`^[a-z0-9-]{1,40}$`), "like eu-central-1")},
	{Key: "s3_access_key", Section: "S3 cache for the runner", Label: "Access key", Kind: KindSecret, When: "s3_server=*", Required: true,
		Secret: SecretS3, SecretKey: "accesskey"},
	{Key: "s3_secret_key", Section: "S3 cache for the runner", Label: "Secret key", Kind: KindSecret, When: "s3_server=*", Required: true,
		Secret: SecretS3, SecretKey: "secretkey"},
}

// ParamByKey is a param by its key.
func ParamByKey(key string) (Param, bool) {
	for _, p := range Params {
		if p.Key == key {
			return p, true
		}
	}
	return Param{}, false
}

// Applies says whether p applies given the other values.
func (p Param) Applies(v map[string]string) bool {
	if p.When == "" {
		return true
	}
	k, want, _ := strings.Cut(p.When, "=")
	if want == "*" {
		return v[k] != ""
	}
	have := v[k]
	if have == "" {
		if q, ok := ParamByKey(k); ok {
			have = q.Default
		}
	}
	return have == want
}

// FieldError is a param's value refused, for the form.
type FieldError struct {
	Key string `json:"key"`
	Msg string `json:"message"`
}

func (e FieldError) Error() string { return e.Key + ": " + e.Msg }

// SecretsMadeByHand counts every secret as given, for CheckSettings on a
// hub that keeps no secrets (no secrets key): the cluster's Secrets are
// then made in the cluster by hand, and the hub asks for none.
func SecretsMadeByHand() map[string]bool {
	m := map[string]bool{}
	for _, p := range Params {
		if p.Kind == KindSecret {
			m[p.Key] = true
		}
	}
	return m
}

// CheckSettings checks a cluster's values (secrets included; a secret
// kept already counts as given when kept[key]): every refused one. A
// secret given for a param that does not apply is refused (it would be
// dropped); other values that do not apply are left out of the settings.
func CheckSettings(values map[string]string, kept map[string]bool) []FieldError {
	var bad []FieldError
	for k := range values {
		if _, ok := ParamByKey(k); !ok {
			bad = append(bad, FieldError{k, "no such setting"})
		}
	}
	for _, p := range Params {
		v := strings.TrimSpace(values[p.Key])
		if !p.Applies(values) {
			if v != "" && p.Kind == KindSecret {
				k, _, _ := strings.Cut(p.When, "=")
				bad = append(bad, FieldError{p.Key, "applies only with " + k})
			}
			continue
		}
		if v == "" {
			if p.Required && p.Default == "" && !(p.Kind == KindSecret && kept[p.Key]) {
				bad = append(bad, FieldError{p.Key, "required"})
			}
			continue
		}
		if p.Kind == KindChoice {
			ok := false
			for _, c := range p.Choices {
				ok = ok || c[0] == v
			}
			if !ok {
				bad = append(bad, FieldError{p.Key, "not one of the choices"})
				continue
			}
		}
		if p.check != nil {
			if err := p.check(v); err != nil {
				bad = append(bad, FieldError{p.Key, err.Error()})
			}
		}
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].Key < bad[j].Key })
	return bad
}

// ---- sealing ----

// Sealer seals the secrets the hub keeps (AES-256-GCM), with a key from a
// Secret the user makes (the chart never makes credentials).
type Sealer struct{ aead cipher.AEAD }

// NewSealer makes a Sealer from key material of at least 32 bytes.
func NewSealer(material []byte) (*Sealer, error) {
	m := strings.TrimSpace(string(material))
	if len(m) < 32 {
		return nil, errors.New("the secrets key must be at least 32 characters (openssl rand -base64 32)")
	}
	k := sha256.Sum256([]byte("ci-accelerator fleet secrets\n" + m))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead}, nil
}

func (s *Sealer) seal(member, key, value string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(value), []byte(member+"\x00"+key))
}

func (s *Sealer) open(member, key string, sealed []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed value too short")
	}
	b, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(member+"\x00"+key))
	if err != nil {
		return "", errors.New("a kept secret does not open: the secrets key was changed")
	}
	return string(b), nil
}

// ---- storage ----

const settingsSchema = `
CREATE TABLE IF NOT EXISTS cluster_settings (
	member     TEXT PRIMARY KEY,
	params     TEXT NOT NULL,
	version    INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	updated_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cluster_secrets (
	member       TEXT NOT NULL,
	param        TEXT NOT NULL,
	sealed       BLOB NOT NULL,
	updated_at   INTEGER NOT NULL,
	updated_by   TEXT NOT NULL,
	delivered_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (member, param)
);
`

// Settings are a cluster's settings as the hub keeps them: its values,
// and which secrets are kept (never their values).
type Settings struct {
	Member    string            `json:"member"`
	Values    map[string]string `json:"values"`
	Version   int               `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	CreatedBy string            `json:"created_by"`
	UpdatedAt time.Time         `json:"updated_at"`
	UpdatedBy string            `json:"updated_by"`
	Secrets   []KeptSecret      `json:"secrets"`
	// RegisteredByKey: the cluster is a member registered by its public
	// key (through the API or the console), not by a join token; its
	// values name the key's Secret, whatever "joins" says.
	RegisteredByKey bool `json:"-"`
}

// KeptSecret is a secret the hub keeps for a cluster: when it was set
// and delivered, never its value.
type KeptSecret struct {
	Param       string    `json:"param"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
	// Stale: it does not open with the hub's secrets key (the key was
	// changed); it is not delivered until it is entered again.
	Stale bool `json:"stale,omitempty"`
}

// Kept says which secrets are kept, by param.
func (s Settings) Kept() map[string]bool {
	m := map[string]bool{}
	for _, k := range s.Secrets {
		m[k.Param] = true
	}
	return m
}

// SaveSettings checks and keeps a cluster's settings: values (secrets
// among them: an empty secret keeps the one kept). It needs a Sealer for
// any secret given; without one no secret is asked for (the cluster's
// Secrets are made there by hand). A secret no longer applying (S3
// turned off) is dropped. Saving what is kept already changes nothing.
func (r *Registry) SaveSettings(ctx context.Context, sealer *Sealer, actor, member string, values map[string]string) (Settings, error) {
	return r.saveSettings(ctx, sealer, actor, member, values, nil)
}

// saveSettings is SaveSettings that also forgets the kept secrets named
// in forget (optional ones only).
func (r *Registry) saveSettings(ctx context.Context, sealer *Sealer, actor, member string, values map[string]string, forget []string) (Settings, error) {
	if !NamePattern.MatchString(member) {
		return Settings{}, fmt.Errorf("%w: a name is lower-case letters, digits and dashes, at most 40, starting and ending with a letter or digit", ErrInvalid)
	}
	old, err := r.Settings(ctx, member)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Settings{}, err
	}
	clean := map[string]string{}
	for k, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			clean[k] = v
		}
	}
	kept := old.Kept()
	if sealer == nil {
		kept = SecretsMadeByHand()
	} else {
		// A secret that does not open is not kept: it must be given again.
		stale, err := r.staleSecrets(ctx, sealer, member)
		if err != nil {
			return Settings{}, err
		}
		for _, k := range stale {
			delete(kept, k)
		}
	}
	var bad []FieldError
	forgotten := map[string]bool{}
	for _, k := range forget {
		p, ok := ParamByKey(k)
		switch {
		case !ok || p.Kind != KindSecret:
			bad = append(bad, FieldError{k, "not a secret"})
		case clean[k] != "":
			bad = append(bad, FieldError{k, "given and to be forgotten"})
		case p.Required && p.Applies(clean):
			bad = append(bad, FieldError{k, "required: give another one instead"})
		default:
			forgotten[k] = true
			delete(kept, k)
		}
	}
	bad = append(bad, CheckSettings(clean, kept)...)
	if f, ok := r.otherKey(ctx, member, clean); ok {
		bad = append(bad, f)
	}
	if len(bad) > 0 {
		sort.Slice(bad, func(i, j int) bool { return bad[i].Key < bad[j].Key })
		msgs := make([]string, len(bad))
		for i, b := range bad {
			msgs[i] = b.Error()
		}
		return Settings{}, &SettingsError{Fields: bad, msg: strings.Join(msgs, "; ")}
	}
	plain := map[string]string{}
	secrets := map[string]string{}
	for _, p := range Params {
		v, ok := clean[p.Key]
		if !ok || !p.Applies(clean) {
			continue
		}
		if p.Kind == KindSecret {
			secrets[p.Key] = v
		} else {
			plain[p.Key] = v
		}
	}
	if len(secrets) > 0 && sealer == nil {
		return Settings{}, fmt.Errorf("%w: this hub keeps no secrets: give it a secrets key (fleet.secretsKey) to send tokens and keys to clusters", ErrInvalid)
	}
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	sealed, err := sealedSecrets(ctx, tx, member)
	if err != nil {
		return Settings{}, err
	}
	var changed []string
	for k, v := range secrets {
		if b, ok := sealed[k]; ok {
			if was, err := sealer.open(member, k, b); err == nil && was == v {
				// The same value: nothing to deliver again.
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cluster_secrets (member, param, sealed, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(member, param) DO UPDATE SET sealed = excluded.sealed, updated_at = excluded.updated_at, updated_by = excluded.updated_by, delivered_at = 0`,
			member, k, sealer.seal(member, k, v), now.UnixMilli(), actor); err != nil {
			return Settings{}, err
		}
		changed = append(changed, k)
	}
	// Secrets of params that no longer apply go, and those to forget.
	for _, k := range old.Secrets {
		p, _ := ParamByKey(k.Param)
		gone := ""
		switch {
		case forgotten[k.Param]:
			gone = " (forgotten)"
		case !p.Applies(clean):
			gone = " (dropped)"
		default:
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cluster_secrets WHERE member = ? AND param = ?`, member, k.Param); err != nil {
			return Settings{}, err
		}
		changed = append(changed, k.Param+gone)
	}
	if old.Member != "" && len(changed) == 0 && sameValues(old.Values, plain) {
		return old, nil
	}
	pj, _ := json.Marshal(plain)
	if _, err := tx.ExecContext(ctx, `INSERT INTO cluster_settings (member, params, version, created_at, created_by, updated_at, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(member) DO UPDATE SET params = excluded.params, version = excluded.version, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		member, string(pj), ParamsVersion, now.UnixMilli(), actor, now.UnixMilli(), actor); err != nil {
		return Settings{}, err
	}
	sort.Strings(changed)
	action := "settings.change"
	if old.Member == "" {
		action = "settings.create"
	}
	detail := "settings v" + strconv.Itoa(ParamsVersion)
	if len(changed) > 0 {
		detail += "; secrets saved: " + strings.Join(changed, ", ")
	}
	if err := audit(ctx, tx, now, actor, action, member, detail); err != nil {
		return Settings{}, err
	}
	if err := tx.Commit(); err != nil {
		return Settings{}, err
	}
	return r.Settings(ctx, member)
}

func sameValues(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// otherKey refuses a public key in the settings that is not the one the
// member of that name is registered with.
func (r *Registry) otherKey(ctx context.Context, member string, values map[string]string) (FieldError, bool) {
	if values["joins"] != "key" || values["public_key"] == "" {
		return FieldError{}, false
	}
	m, err := r.Get(ctx, member)
	if err != nil || !m.RemovedAt.IsZero() {
		return FieldError{}, false
	}
	keys, err := ParsePublicKeys([]byte(values["public_key"]))
	if err != nil || len(keys) != 1 || Fingerprint(keys[0]) == m.Fingerprint {
		return FieldError{}, false
	}
	return FieldError{"public_key", "not the key " + member + " is registered with: remove the member to register another"}, true
}

func sealedSecrets(ctx context.Context, tx *sql.Tx, member string) (map[string][]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT param, sealed FROM cluster_secrets WHERE member = ?`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, rows.Err()
}

// SettingsError lists the values a save refused.
type SettingsError struct {
	Fields []FieldError
	msg    string
}

func (e *SettingsError) Error() string { return ErrInvalid.Error() + ": " + e.msg }
func (e *SettingsError) Unwrap() error { return ErrInvalid }

// ErrNoSettings: a cluster has no settings at the hub (a member may be
// registered under the name all the same).
var ErrNoSettings = fmt.Errorf("%w: no settings at the hub for this cluster", ErrNotFound)

// Settings are a cluster's settings; ErrNoSettings (an ErrNotFound) when
// it has none.
func (r *Registry) Settings(ctx context.Context, member string) (Settings, error) {
	if member == "" {
		return Settings{}, ErrNoSettings
	}
	ss, err := r.querySettings(ctx, member)
	if err != nil {
		return Settings{}, err
	}
	if len(ss) == 0 {
		return Settings{}, ErrNoSettings
	}
	return ss[0], nil
}

// SettingsList are the settings of every cluster that has some, by name.
func (r *Registry) SettingsList(ctx context.Context) ([]Settings, error) {
	return r.querySettings(ctx, "")
}

// querySettings reads one cluster's settings, or every cluster's ("").
func (r *Registry) querySettings(ctx context.Context, member string) ([]Settings, error) {
	where, args := "", []any{}
	if member != "" {
		where, args = " WHERE member = ?", []any{member}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT member, params, version, created_at, created_by, updated_at, updated_by FROM cluster_settings`+where+` ORDER BY member`, args...)
	if err != nil {
		return nil, err
	}
	out := []Settings{}
	at := map[string]int{}
	for rows.Next() {
		var s Settings
		var pj string
		var created, updated int64
		if err := rows.Scan(&s.Member, &pj, &s.Version, &created, &s.CreatedBy, &updated, &s.UpdatedBy); err != nil {
			rows.Close()
			return nil, err
		}
		s.CreatedAt, s.UpdatedAt = millis(created), millis(updated)
		if err := json.Unmarshal([]byte(pj), &s.Values); err != nil {
			rows.Close()
			return nil, err
		}
		s.Secrets = []KeptSecret{}
		at[s.Member] = len(out)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	rows, err = r.db.QueryContext(ctx, `SELECT member, param, updated_at, updated_by, delivered_at FROM cluster_secrets`+where+` ORDER BY member, param`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m string
		var k KeptSecret
		var up, del int64
		if err := rows.Scan(&m, &k.Param, &up, &k.UpdatedBy, &del); err != nil {
			rows.Close()
			return nil, err
		}
		k.UpdatedAt, k.DeliveredAt = millis(up), millis(del)
		if i, ok := at[m]; ok {
			out[i].Secrets = append(out[i].Secrets, k)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	q := `SELECT name FROM members WHERE removed_at = 0 AND origin != ?`
	if member != "" {
		q += ` AND name = ?`
	}
	rows, err = r.db.QueryContext(ctx, q, append([]any{OriginJoin}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if i, ok := at[n]; ok {
			out[i].RegisteredByKey = true
		}
	}
	return out, rows.Err()
}

// DeleteSettings forgets a cluster's settings and secrets.
func (r *Registry) DeleteSettings(ctx context.Context, actor, member string) error {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if n, err := deleteSettings(ctx, tx, member); err != nil || n == 0 {
		return err
	}
	if err := audit(ctx, tx, now, actor, "settings.delete", member, "settings and secrets forgotten"); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteSettings deletes a cluster's settings and secrets: how many rows.
func deleteSettings(ctx context.Context, tx *sql.Tx, member string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM cluster_settings WHERE member = ?`, member)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `DELETE FROM cluster_secrets WHERE member = ?`, member)
	if err != nil {
		return 0, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// RemoveCluster takes a cluster out of the fleet in one transaction: its
// member is removed (the name stays taken KeepRemoved), its unused join
// tokens are revoked, and its settings and kept secrets are forgotten, so
// that nothing of it goes to a cluster later registered under the name.
// removed: a member was removed (the caller closes its link). ErrNotFound
// when the hub had nothing of the name.
func (r *Registry) RemoveCluster(ctx context.Context, actor, name string) (removed bool, err error) {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE members SET removed_at = ? WHERE name = ? AND removed_at = 0`, now.UnixMilli(), name)
	if err != nil {
		return false, err
	}
	members, _ := res.RowsAffected()
	if members > 0 {
		if err := audit(ctx, tx, now, actor, "member.remove", name, ""); err != nil {
			return false, err
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE member = ? AND used_at = 0 AND revoked_at = 0 AND expires_at > ?`,
		now.UnixMilli(), name, now.UnixMilli())
	if err != nil {
		return false, err
	}
	joins, _ := res.RowsAffected()
	if joins > 0 {
		if err := audit(ctx, tx, now, actor, "join-token.revoke", name, fmt.Sprintf("%d unused, the cluster removed", joins)); err != nil {
			return false, err
		}
	}
	settings, err := deleteSettings(ctx, tx, name)
	if err != nil {
		return false, err
	}
	if settings > 0 {
		if err := audit(ctx, tx, now, actor, "settings.delete", name, "settings and secrets forgotten"); err != nil {
			return false, err
		}
	}
	if members == 0 && joins == 0 && settings == 0 {
		return false, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if members == 0 {
		return false, nil
	}
	r.committed(ctx, r.reload)
	return true, nil
}

// Delivery is the Secrets to write in a member: name -> key -> value.
type Delivery map[string]map[string]string

// SecretsFor opens a cluster's kept secrets into the Secrets the member
// writes; an error when one does not open (Outgoing leaves those out).
func (r *Registry) SecretsFor(ctx context.Context, sealer *Sealer, member string) (Delivery, error) {
	o, err := r.Outgoing(ctx, sealer, member)
	if err != nil {
		return nil, err
	}
	if len(o.Stale) > 0 {
		return nil, errors.New("a kept secret does not open: the secrets key was changed")
	}
	return o.Secrets, nil
}

// Outgoing is a delivery of a member's kept secrets as read at one time.
type Outgoing struct {
	Secrets Delivery
	// Stale: kept secrets (params) that do not open with the hub's
	// secrets key (it was changed); left out until entered again.
	Stale []string
	// sealed: what was read of each secret sent, param -> sealed value.
	sealed map[string][]byte
}

// Outgoing opens a cluster's kept secrets into the Secrets the member
// writes, leaving out (and naming) the ones that do not open.
func (r *Registry) Outgoing(ctx context.Context, sealer *Sealer, member string) (Outgoing, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT param, sealed FROM cluster_secrets WHERE member = ? ORDER BY param`, member)
	if err != nil {
		return Outgoing{}, err
	}
	defer rows.Close()
	o := Outgoing{Secrets: Delivery{}, sealed: map[string][]byte{}}
	for rows.Next() {
		var k string
		var sealed []byte
		if err := rows.Scan(&k, &sealed); err != nil {
			return Outgoing{}, err
		}
		p, ok := ParamByKey(k)
		if !ok || p.Secret == "" {
			continue
		}
		if sealer == nil {
			return Outgoing{}, errors.New("the hub has no secrets key: its kept secrets cannot be opened")
		}
		v, err := sealer.open(member, k, sealed)
		if err != nil {
			o.Stale = append(o.Stale, k)
			continue
		}
		if o.Secrets[p.Secret] == nil {
			o.Secrets[p.Secret] = map[string]string{}
		}
		o.Secrets[p.Secret][p.SecretKey] = v
		o.sealed[k] = sealed
	}
	if err := rows.Err(); err != nil {
		return Outgoing{}, err
	}
	if s := o.Secrets[SecretRunner]; s != nil {
		// The runner chart mounts this key even when empty.
		s["runner-registration-token"] = ""
	}
	return o, nil
}

// staleSecrets are a cluster's kept secrets that do not open with sealer.
func (r *Registry) staleSecrets(ctx context.Context, sealer *Sealer, member string) ([]string, error) {
	o, err := r.Outgoing(ctx, sealer, member)
	return o.Stale, err
}

// MarkStale sets Stale on the kept secrets of s that do not open with
// sealer (none without one).
func (r *Registry) MarkStale(ctx context.Context, sealer *Sealer, s *Settings) error {
	if sealer == nil {
		return nil
	}
	stale, err := r.staleSecrets(ctx, sealer, s.Member)
	if err != nil {
		return err
	}
	for i := range s.Secrets {
		for _, k := range stale {
			s.Secrets[i].Stale = s.Secrets[i].Stale || s.Secrets[i].Param == k
		}
	}
	return nil
}

// Delivered records that a member wrote its Secrets: every secret not
// delivered yet. DeliveredOutgoing records only what a delivery carried.
func (r *Registry) Delivered(ctx context.Context, member string, names []string) error {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE cluster_secrets SET delivered_at = ? WHERE member = ? AND delivered_at = 0`, now.UnixMilli(), member); err != nil {
		return err
	}
	sort.Strings(names)
	if err := audit(ctx, tx, now, "member:"+member, "secrets.deliver", member, "written: "+strings.Join(names, ", ")); err != nil {
		return err
	}
	return tx.Commit()
}

// DeliveredOutgoing records that a member wrote the Secrets named in
// written from o: a secret counts as delivered only when o carried it, its
// Secret is among those written, and it still holds the value o carried
// (one saved since stays undelivered). Nothing marked, nothing audited.
func (r *Registry) DeliveredOutgoing(ctx context.Context, member string, o Outgoing, written []string) error {
	wrote := map[string]bool{}
	for _, n := range written {
		wrote[n] = true
	}
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marked int64
	for k, sealed := range o.sealed {
		if p, _ := ParamByKey(k); !wrote[p.Secret] {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE cluster_secrets SET delivered_at = ? WHERE member = ? AND param = ? AND sealed = ? AND delivered_at = 0`,
			now.UnixMilli(), member, k, sealed)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		marked += n
	}
	if marked == 0 {
		return nil
	}
	names := append([]string(nil), written...)
	sort.Strings(names)
	if err := audit(ctx, tx, now, "member:"+member, "secrets.deliver", member, "written: "+strings.Join(names, ", ")); err != nil {
		return err
	}
	return tx.Commit()
}

// Undelivered says whether a member has secrets not delivered yet.
func (r *Registry) Undelivered(ctx context.Context, member string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM cluster_secrets WHERE member = ? AND delivered_at = 0`, member).Scan(&n)
	return n > 0, err
}
