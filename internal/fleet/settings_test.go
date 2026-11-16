package fleet

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the generated values' golden file")

// The values a cluster's settings generate: the golden file make validate
// renders with the box's chart (charts/ci-accelerator).
const goldenValues = "../../charts/ci-accelerator/tests/values/fleet-member.yaml"

func testSettings(t *testing.T, r *Registry, sealer *Sealer) Settings {
	t.Helper()
	s, err := r.SaveSettings(context.Background(), sealer, "gitlab:g/1", "prod-ap", map[string]string{
		"joins": "token", "token_hours": "24", "storage": "hostPath", "host_path": "/var/mnt/nvme/buildkit", "cache_limit": "400GB",
		"gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-not-a-real-token", "concurrent": "40",
		"gitlab_token": "glpat-readonly-xxxxxxxx", "s3_server": "s3.eu-central-1.amazonaws.com", "s3_bucket": "ci-cache-prod-ap",
		"s3_region": "eu-central-1", "s3_access_key": "AKIAEXAMPLE", "s3_secret_key": "very-secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sealer, err := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Error("a short secrets key accepted")
	}
	s := testSettings(t, r, sealer)
	if len(s.Secrets) != 4 || s.Values["runner_token"] != "" || s.Values["host_path"] != "/var/mnt/nvme/buildkit" {
		t.Fatalf("kept: %+v", s)
	}
	// Secrets: nowhere in the file in the clear; they open into the
	// Secrets the member writes.
	r.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, f := range []string{"fleet.db", "fleet.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		for _, secret := range []string{"glrt-not-a-real-token", "very-secret-key", "AKIAEXAMPLE"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%q is in %s in the clear", secret, f)
			}
		}
	}
	d, err := r.SecretsFor(ctx, sealer, "prod-ap")
	if err != nil {
		t.Fatal(err)
	}
	if d[SecretRunner]["runner-token"] != "glrt-not-a-real-token" || d[SecretRunner]["runner-registration-token"] != "" ||
		d[SecretS3]["accesskey"] != "AKIAEXAMPLE" || d[SecretS3]["secretkey"] != "very-secret-key" || d[SecretGitLab]["token"] != "glpat-readonly-xxxxxxxx" {
		t.Errorf("delivery: %v", d)
	}
	other, _ := NewSealer([]byte("another-key-another-key-another-key"))
	if _, err := r.SecretsFor(ctx, other, "prod-ap"); err == nil {
		t.Error("secrets open with another key")
	}
	if u, _ := r.Undelivered(ctx, "prod-ap"); !u {
		t.Error("nothing undelivered after a save")
	}
	if err := r.Delivered(ctx, "prod-ap", []string{SecretRunner, SecretS3}); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.Undelivered(ctx, "prod-ap"); u {
		t.Error("undelivered after delivery")
	}

	// A change: empty secrets keep the kept ones; S3 off drops its keys.
	s2, err := r.SaveSettings(ctx, sealer, "gitlab:g/2", "prod-ap", map[string]string{
		"joins": "token", "storage": "emptyDir", "gitlab_url": "https://gitlab.example.com/", "concurrent": "8"})
	if err != nil {
		t.Fatal(err)
	}
	if k := s2.Kept(); !k["runner_token"] || !k["gitlab_token"] || k["s3_access_key"] || k["s3_secret_key"] {
		t.Errorf("kept after the change: %v", k)
	}
	if u, _ := r.Undelivered(ctx, "prod-ap"); u {
		t.Error("a change without new secrets made them undelivered")
	}
	// A secret replaced: delivered again.
	if _, err := r.SaveSettings(ctx, sealer, "gitlab:g/2", "prod-ap", map[string]string{
		"joins": "token", "storage": "emptyDir", "gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-replaced-0123456789"}); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.Undelivered(ctx, "prod-ap"); !u {
		t.Error("a replaced secret counts as delivered")
	}
	// Refusals, per field.
	_, err = r.SaveSettings(ctx, sealer, "x", "eu", map[string]string{"joins": "key", "public_key": "nope", "storage": "hostPath", "host_path": "relative",
		"cache_limit": "400Gi", "gitlab_url": "gitlab.example.com", "runner_token": "token", "concurrent": "0", "s3_server": "https://s3", "bogus": "1"})
	var se *SettingsError
	if !errors.As(err, &se) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("refused: %v", err)
	}
	var keys []string
	for _, f := range se.Fields {
		keys = append(keys, f.Key)
	}
	want := "bogus cache_limit concurrent gitlab_url host_path public_key runner_token s3_access_key s3_bucket s3_region s3_secret_key s3_server"
	if strings.Join(keys, " ") != want {
		t.Errorf("refused fields:\n%s\nwant\n%s", strings.Join(keys, " "), want)
	}
	// Secrets need a sealer.
	if _, err := r.SaveSettings(ctx, nil, "x", "eu", map[string]string{"gitlab_url": "https://g/", "runner_token": "glrt-not-a-real-token"}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "keeps no secrets") {
		t.Errorf("a secret without a sealer: %v", err)
	}
	es, _ := r.AuditQuery(ctx, AuditQuery{Target: "prod-ap"})
	var acts []string
	for _, e := range es {
		acts = append(acts, e.Action+" "+e.Detail)
	}
	if len(acts) != 4 || !strings.Contains(acts[1], "s3_access_key (dropped)") || !strings.Contains(acts[3], "secrets saved: gitlab_token, runner_token, s3_access_key, s3_secret_key") {
		t.Errorf("audit: %q", acts)
	}
	for _, a := range acts {
		for _, secret := range []string{"glrt-not-a-real-token", "very-secret-key", "AKIAEXAMPLE"} {
			if strings.Contains(a, secret) {
				t.Errorf("a secret in the audit: %s", a)
			}
		}
	}
}

// The generated values are the golden file make validate renders with the
// box's chart; go test ./internal/fleet -run TestGenerate -update writes it.
func TestGenerate(t *testing.T) {
	r, _ := openReg(t)
	sealer, _ := NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	s := testSettings(t, r, sealer)
	hub := HubInfo{URL: "https://hub.example.com", PublicKeys: "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAhub\n-----END PUBLIC KEY-----\n"}
	in := Generate(s, hub, "0.8.0", "cia_join_SECRET", false)
	for _, want := range []string{"kubectl create namespace ci-accelerator", "--from-literal=token=cia_join_SECRET",
		"helm upgrade --install ci-accelerator " + ChartRef + " --version 0.8.0 \\\n  -n ci-accelerator -f prod-ap.values.yaml"} {
		if !strings.Contains(strings.Join(in.Commands, "\n"), want) {
			t.Errorf("commands: no %q", want)
		}
	}
	for _, secret := range []string{"glrt-", "very-secret-key", "AKIAEXAMPLE", "cia_join_"} {
		if strings.Contains(in.Values, secret) {
			t.Errorf("a secret in the values: %s", secret)
		}
	}
	up := Generate(s, hub, "0.8.0", "", true)
	if strings.Contains(strings.Join(up.Commands, "\n"), "kubectl") || len(up.Commands) != 1 {
		t.Errorf("an upgrade's commands: %q", up.Commands)
	}
	if *update {
		if err := os.WriteFile(goldenValues, []byte(in.Values), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(goldenValues)
	if err != nil {
		t.Fatalf("%v (go test ./internal/fleet -run TestGenerate -update writes it)", err)
	}
	if string(golden) != in.Values {
		t.Errorf("the generated values differ from %s (make validate renders that file; -update writes it):\n%s", goldenValues, in.Values)
	}
}

// Two clusters' secrets stay apart: a delivery holds only its own
// cluster's values, a sealed value does not open under another cluster
// (or another param), recording one cluster's delivery leaves the other's
// alone, and sealing the same value twice gives different ciphertexts.
func TestSecretsOfTwoMembers(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	sealer, err := NewSealer([]byte(testSecretsKey))
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]map[string]string{
		"a": {"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir", "runner_token": "glrt-of-a-aaaaaaaaaaaa", "gitlab_token": "glpat-of-a-aaaaaaaa"},
		"b": {"gitlab_url": "https://gitlab.example.com/", "storage": "emptyDir", "runner_token": "glrt-of-b-bbbbbbbbbbbb", "gitlab_token": "glpat-of-b-bbbbbbbb"},
	}
	for _, m := range []string{"a", "b"} {
		if _, err := r.SaveSettings(ctx, sealer, "x", m, vals[m]); err != nil {
			t.Fatal(m, err)
		}
	}
	da, err := r.SecretsFor(ctx, sealer, "a")
	if err != nil {
		t.Fatal(err)
	}
	if da[SecretRunner]["runner-token"] != vals["a"]["runner_token"] {
		t.Fatalf("a's delivery: %v", da)
	}
	for _, kv := range da {
		for _, v := range kv {
			if strings.Contains(v, "-of-b-") {
				t.Fatalf("a's delivery holds b's value %q", v)
			}
		}
	}

	// a's sealed runner token copied into b's row: it is a's, not b's.
	var sealedA []byte
	if err := r.db.QueryRowContext(ctx, `SELECT sealed FROM cluster_secrets WHERE member = 'a' AND param = 'runner_token'`).Scan(&sealedA); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE cluster_secrets SET sealed = ? WHERE member = 'b' AND param = 'runner_token'`, sealedA); err != nil {
		t.Fatal(err)
	}
	ob, err := r.Outgoing(ctx, sealer, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(ob.Stale) != 1 || ob.Stale[0] != "runner_token" || ob.Secrets[SecretRunner]["runner-token"] != "" {
		t.Fatalf("a's sealed value under b: stale %v, delivery %v", ob.Stale, ob.Secrets)
	}
	if _, err := r.SecretsFor(ctx, sealer, "b"); err == nil {
		t.Fatal("SecretsFor(b) opened a's sealed value")
	}
	if _, err := sealer.open("a", "gitlab_token", sealedA); err == nil {
		t.Fatal("a's runner token opened as a's gitlab token")
	}

	if err := r.Delivered(ctx, "a", []string{SecretRunner}); err != nil {
		t.Fatal(err)
	}
	if u, err := r.Undelivered(ctx, "a"); err != nil || u {
		t.Fatalf("a undelivered after Delivered(a): %v %v", u, err)
	}
	if u, err := r.Undelivered(ctx, "b"); err != nil || !u {
		t.Fatalf("b delivered by Delivered(a): undelivered=%v %v", u, err)
	}

	one, two := sealer.seal("a", "runner_token", "same"), sealer.seal("a", "runner_token", "same")
	if string(one) == string(two) {
		t.Fatal("two seals of the same value are the same ciphertext")
	}
}
