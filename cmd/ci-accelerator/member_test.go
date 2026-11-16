package main

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

func memberTestLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// joinHubServer is a hub's join endpoint over TLS, in front of which
// before may answer first (true: answered).
func joinHubServer(t *testing.T, before func(w http.ResponseWriter, r *http.Request) bool) (*fleet.Registry, *httptest.Server, *tls.Config) {
	t.Helper()
	reg, err := fleet.OpenRegistry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	join := reg.JoinHandler()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if before != nil && before(w, r) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == fleet.JoinPath {
			join.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return reg, ts, &tls.Config{RootCAs: pool}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func fastJoinRetries(t *testing.T) {
	was := joinWait
	joinWait = func(int, bool) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { joinWait = was })
}

// A hub that is away when the box starts (503 from its Ingress, a hub
// being rolled) does not leave the box out: the token is traded again
// until the hub answers.
func TestJoinRetriedUntilTheHubAnswers(t *testing.T) {
	fastJoinRetries(t)
	var calls atomic.Int32
	reg, ts, outer := joinHubServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if calls.Add(1) <= 3 {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, secret, err := reg.IssueJoin(ctx, "gitlab:g/1", "eu", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "token"), []byte(secret), 0o600)
	c := config{fleetHub: ts.URL, fleetName: "eu", fleetKey: filepath.Join(dir, "absent"), fleetJoin: filepath.Join(dir, "token"),
		fleetKeySecret: "fleet-key", namespace: "box"}
	key, err := memberKey(ctx, c, kubefake.NewClientset(), outer, memberTestLog())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reg.Holder(fleet.Fingerprint(key.Public())); err != nil || n != "eu" {
		t.Fatalf("after the hub came back: registered %q (%v), %d join calls", n, err, calls.Load())
	}
}

// A refused token (expired, revoked) is asked again with what the token
// file holds then: a new token put into the Secret counts without a
// restart.
func TestJoinReadsANewToken(t *testing.T) {
	fastJoinRetries(t)
	reg, ts, outer := joinHubServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old, oldSecret, _ := reg.IssueJoin(ctx, "gitlab:g/1", "eu", 0)
	if err := reg.RevokeJoin(ctx, "gitlab:g/1", "eu", old.ID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte(oldSecret), 0o600)
	c := config{fleetHub: ts.URL, fleetName: "eu", fleetKey: filepath.Join(dir, "absent"), fleetJoin: tokenFile,
		fleetKeySecret: "fleet-key", namespace: "box"}
	type result struct {
		fp  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		key, err := memberKey(ctx, c, kubefake.NewClientset(), outer, memberTestLog())
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{fp: fleet.Fingerprint(key.Public())}
	}()
	time.Sleep(100 * time.Millisecond)
	_, newSecret, _ := reg.IssueJoin(ctx, "gitlab:g/1", "eu", 0)
	os.WriteFile(tokenFile, []byte(newSecret+"\n"), 0o600)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n, err := reg.Holder(r.fp); err != nil || n != "eu" {
		t.Fatalf("with the new token: registered %q (%v)", n, err)
	}
}

// Taking the key and trading the token are not on the box's start path:
// fleetDialer reads only files.
func TestFleetDialerDoesNotWaitForTheHub(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(time.Minute)
	}))
	defer ts.Close()
	dir := t.TempDir()
	_, pub, _ := fleet.GenerateKey()
	os.WriteFile(filepath.Join(dir, "hub.pub"), pub, 0o644)
	os.WriteFile(filepath.Join(dir, "token"), []byte("cia_join_x"), 0o600)
	c := config{fleetHub: ts.URL, fleetName: "eu", fleetKey: filepath.Join(dir, "absent"), fleetHubKeys: filepath.Join(dir, "hub.pub"),
		fleetJoin: filepath.Join(dir, "token"), fleetKeySecret: "fleet-key", namespace: "box"}
	st := openTestStore(t)
	began := time.Now()
	d, _, err := fleetDialer(c, st, http.NotFoundHandler(), memberTestLog())
	if err != nil {
		t.Fatal(err)
	}
	if d.Key != nil || calls.Load() != 0 || time.Since(began) > 5*time.Second {
		t.Errorf("fleetDialer: key %v, hub calls %d, took %s", d.Key != nil, calls.Load(), time.Since(began))
	}
	// A key file that holds a wrong key is said at start.
	os.WriteFile(filepath.Join(dir, "bad"), []byte("not a key"), 0o600)
	c.fleetKey = filepath.Join(dir, "bad")
	if _, _, err := fleetDialer(c, st, http.NotFoundHandler(), memberTestLog()); err == nil || !strings.Contains(err.Error(), "-fleet-key-file") {
		t.Errorf("a wrong key file: %v", err)
	}
}

// The join token goes to the hub's address only: a redirect is not
// followed (not even to another https host), and a 200 that is not the
// hub's answer for this member and key is not a join.
func TestJoinFollowsNoRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		io.WriteString(w, "<html>sign in</html>")
	}))
	defer other.Close()
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		_, ts, outer := joinHubServer(t, func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, other.URL+"/collect", code)
			return true
		})
		outer.RootCAs.AddCert(other.Certificate())
		priv, _, _ := fleet.GenerateKey()
		key, _ := fleet.ParsePrivateKey(priv)
		err := joinHub(context.Background(), config{fleetHub: ts.URL, fleetName: "eu"}, outer, key, "cia_join_secret")
		if err == nil || elsewhere.Load() != 0 {
			t.Errorf("%d: err %v, the other host saw %d requests", code, err, elsewhere.Load())
		}
	}
	// Any page answering 200 is not a join.
	_, ts, outer := joinHubServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		io.WriteString(w, `{"name":"eu","fingerprint":"SHA256:someone-else"}`)
		return true
	})
	priv, _, _ := fleet.GenerateKey()
	key, _ := fleet.ParsePrivateKey(priv)
	if err := joinHub(context.Background(), config{fleetHub: ts.URL, fleetName: "eu"}, outer, key, "t"); err == nil {
		t.Error("an answer naming another key counted as joined")
	}
}

// The join token is checked against the address's TLS: a hub whose
// certificate the member does not trust gets nothing.
func TestJoinChecksTheHubsCertificate(t *testing.T) {
	var calls atomic.Int32
	reg, ts, _ := joinHubServer(t, func(http.ResponseWriter, *http.Request) bool { calls.Add(1); return false })
	_, secret, _ := reg.IssueJoin(context.Background(), "gitlab:g/1", "eu", 0)
	priv, _, _ := fleet.GenerateKey()
	key, _ := fleet.ParsePrivateKey(priv)
	err := joinHub(context.Background(), config{fleetHub: ts.URL, fleetName: "eu"}, &tls.Config{RootCAs: x509.NewCertPool()}, key, secret)
	if err == nil || calls.Load() != 0 {
		t.Errorf("an untrusted certificate: err %v, the hub saw %d requests", err, calls.Load())
	}
}

// A join token is sent over https only, or over http to this host or a
// Service of the cluster; without a token (a key file) the pinned link
// protects everything.
func TestHubAddress(t *testing.T) {
	for _, tc := range []struct {
		url   string
		token bool
		ok    bool
	}{
		{"https://hub.example.com", true, true},
		{"https://hub.example.com/fleet", true, true},
		{"http://hub.example.com", false, true},
		{"http://hub.example.com", true, false},
		{"http://10.0.0.7:8080", true, false},
		{"http://localhost:8080", true, true},
		{"http://127.0.0.1:8080", true, true},
		{"http://[::1]:8080", true, true},
		{"http://hub-web.fleet.svc:8080", true, true},
		{"http://hub-web.fleet.svc.cluster.local:8080", true, true},
		{"ftp://hub.example.com", false, false},
		{"hub.example.com", false, false},
		{"https://", true, false},
	} {
		if err := hubAddress(tc.url, tc.token); (err == nil) != tc.ok {
			t.Errorf("%s token=%v: %v", tc.url, tc.token, err)
		}
	}
}

// The controller refuses a join token with a plain http hub address
// before it does anything else.
func TestRunRefusesAJoinTokenOverHTTP(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	dir := t.TempDir()
	c := config{data: dir, discover: "buildkit.example:1234", gitlabURLs: []string{"https://gitlab.example.com/"}, keepDays: 1, keepOutputDays: 1,
		split: true, kubeconfig: filepath.Join(dir, "absent"), fleetHub: "http://hub.example.com", fleetName: "eu", fleetKey: filepath.Join(dir, "key"),
		fleetHubKeys: filepath.Join(dir, "hub.pub"), fleetJoin: filepath.Join(dir, "token"), fleetKeySecret: "fleet-key", namespace: "box"}
	err := run(c, memberTestLog())
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http hub address with a join token: %v", err)
	}
}

// A damaged key Secret is named in the error.
func TestKeptKeyNamesADamagedSecret(t *testing.T) {
	kube := kubefake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fleet-key", Namespace: "box"},
		Data: map[string][]byte{"key": []byte("not a key")}})
	_, err := keptKey(context.Background(), kube, "box", "fleet-key", memberTestLog())
	if err == nil || !strings.Contains(err.Error(), "box/fleet-key") {
		t.Fatalf("a damaged key Secret: %v", err)
	}
}

// fleet-keygen never writes over a key: a hub's is pinned by its members.
func TestFleetKeygenKeepsAnExistingKey(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "hub.key")
	if err := fleetKeygen([]string{f}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f)
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
		t.Errorf("the private key's mode: %v", st.Mode())
	}
	if err := fleetKeygen([]string{f}); err == nil {
		t.Error("a second fleet-keygen over the same file: no error")
	}
	if after, _ := os.ReadFile(f); string(after) != string(before) {
		t.Error("the key was written over")
	}
	// A .pub left alone is not written over either.
	g := filepath.Join(dir, "member.key")
	os.WriteFile(g+".pub", []byte("kept"), 0o644)
	if err := fleetKeygen([]string{g}); err == nil {
		t.Error("an existing .pub: no error")
	}
	if _, err := os.Stat(g); err == nil {
		t.Error("a private key was written without its .pub")
	}
}

// putDelivery sends a delivery to w as the hub would.
func putDelivery(t *testing.T, w http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, SecretsPath, strings.NewReader(body)))
	return rec
}

func overTheLink(context.Context) bool { return true }

// The member writes only what its hub may deliver, only for the hub over
// the link, all or nothing checked before the first write; keys the
// Secret's owner added stay.
func TestSecretWriter(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset()
	log := memberTestLog()
	runner := `{"secrets":{"fleet-runner-token":{"runner-token":"glrt-1","runner-registration-token":""}}}`

	// Not over the link: refused, nothing written.
	if r := putDelivery(t, secretWriter(kube, "box", fleet.Over, log), runner); r.Code != http.StatusForbidden {
		t.Fatalf("not over the link: %d", r.Code)
	}
	if _, err := kube.CoreV1().Secrets("box").Get(ctx, fleet.SecretRunner, metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Fatalf("written although refused: %v", err)
	}
	w := secretWriter(kube, "box", overTheLink, log)
	for _, bad := range []struct {
		body string
		code int
	}{
		// A name it may not write, with one it may.
		{`{"secrets":{"fleet-runner-token":{"runner-token":"glrt-1"},"kube-root-ca":{"ca.crt":"x"}}}`, http.StatusForbidden},
		// A key it may not write.
		{`{"secrets":{"fleet-runner-token":{"runner-token":"glrt-1"},"fleet-s3-cache":{"accesskey":"a","note":"x"}}}`, http.StatusBadRequest},
		// Empty data, an empty value.
		{`{"secrets":{"fleet-runner-token":{"runner-token":"glrt-1"},"fleet-s3-cache":{}}}`, http.StatusBadRequest},
		{`{"secrets":{"fleet-runner-token":{"runner-token":"glrt-1"},"fleet-gitlab-token":{"token":""}}}`, http.StatusBadRequest},
	} {
		if r := putDelivery(t, w, bad.body); r.Code != bad.code {
			t.Errorf("%s: %d", bad.body, r.Code)
		}
		if l, _ := kube.CoreV1().Secrets("box").List(ctx, metav1.ListOptions{}); len(l.Items) != 0 {
			t.Fatalf("%s: written before the refusal: %d Secrets", bad.body, len(l.Items))
		}
	}
	// A delivery: created, labelled as the hub's.
	if r := putDelivery(t, w, runner); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), fleet.SecretRunner) {
		t.Fatalf("a delivery: %d %s", r.Code, r.Body)
	}
	s, err := kube.CoreV1().Secrets("box").Get(ctx, fleet.SecretRunner, metav1.GetOptions{})
	if err != nil || string(s.Data["runner-token"]) != "glrt-1" || s.Labels["app.kubernetes.io/managed-by"] != "ci-accelerator" {
		t.Fatalf("the Secret: %v %v", s, err)
	}
	// The owner added a key; the next delivery replaces the token, keeps it.
	s.Data["note"] = []byte("mine")
	kube.CoreV1().Secrets("box").Update(ctx, s, metav1.UpdateOptions{})
	if r := putDelivery(t, w, strings.Replace(runner, "glrt-1", "glrt-2", 1)); r.Code != http.StatusOK {
		t.Fatalf("again: %d %s", r.Code, r.Body)
	}
	s, _ = kube.CoreV1().Secrets("box").Get(ctx, fleet.SecretRunner, metav1.GetOptions{})
	if string(s.Data["runner-token"]) != "glrt-2" || string(s.Data["note"]) != "mine" {
		t.Errorf("after the second delivery: %q", s.Data)
	}
}

// Every Secret the hub delivers has its keys listed here: a name added
// to fleet.DeliveredSecrets without them would be refused.
func TestDeliveredKeysCoverEverySecret(t *testing.T) {
	for _, n := range fleet.DeliveredSecrets {
		if len(deliveredKeys[n]) == 0 {
			t.Errorf("%s: no keys", n)
		}
	}
}

// An update that loses a race is tried again; a write that fails still
// answers which Secrets were written.
func TestSecretWriterConflictAndPartial(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fleet.SecretGitLab, Namespace: "box"},
		Data: map[string][]byte{"token": []byte("old")}})
	var conflicts atomic.Int32
	kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts.Add(1) == 1 {
			return true, nil, kerrors.NewConflict(corev1.Resource("secrets"), fleet.SecretGitLab, errors.New("changed meanwhile"))
		}
		return false, nil, nil
	})
	w := secretWriter(kube, "box", overTheLink, memberTestLog())
	if r := putDelivery(t, w, `{"secrets":{"fleet-gitlab-token":{"token":"new"}}}`); r.Code != http.StatusOK {
		t.Fatalf("after a conflict: %d %s", r.Code, r.Body)
	}
	if s, _ := kube.CoreV1().Secrets("box").Get(ctx, fleet.SecretGitLab, metav1.GetOptions{}); string(s.Data["token"]) != "new" {
		t.Errorf("after a conflict: %q", s.Data["token"])
	}
	// The S3 Secret cannot be written: the answer names what was.
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Name == fleet.SecretS3 {
			return true, nil, errors.New("admission webhook said no")
		}
		return false, nil, nil
	})
	r := putDelivery(t, w, `{"secrets":{"fleet-gitlab-token":{"token":"newer"},"fleet-s3-cache":{"accesskey":"a","secretkey":"s"}}}`)
	var out struct {
		Written []string `json:"written"`
		Error   string   `json:"error"`
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	if r.Code != http.StatusInternalServerError || len(out.Written) != 1 || out.Written[0] != fleet.SecretGitLab || out.Error == "" {
		t.Errorf("a failed write: %d %s", r.Code, r.Body)
	}
}

// A box on its own starts with an empty GitLab token file, as before
// fleets (GitLab then refuses the reads); a missing file is refused.
func TestGitLabTokenFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte(" \n"), 0o600)
	ctx := context.Background()
	if _, err := gitlabToken(ctx, config{gitlabTokenFile: empty}, memberTestLog()); err != nil {
		t.Errorf("an empty token file: %v", err)
	}
	if _, err := gitlabToken(ctx, config{gitlabTokenFile: filepath.Join(dir, "absent")}, memberTestLog()); err == nil {
		t.Error("a missing token file: no error")
	}
	if _, err := gitlabToken(ctx, config{gitlabTokenFile: filepath.Join(dir, "absent"), fleetHub: "https://hub.example.com"}, memberTestLog()); err != nil {
		t.Errorf("a member before its hub delivered the token: %v", err)
	}
}

// The install commands of a hub run without its chart name the bare
// version, never the commit.
func TestChartVersionWithoutCommit(t *testing.T) {
	t.Setenv("CI_ACCELERATOR_CHART_VERSION", "")
	wv, wc := version, commit
	defer func() { version, commit = wv, wc }()
	version, commit = "v0.7.0", "0921bbc"
	if v := chartVersion(); v != "0.7.0" {
		t.Errorf("chartVersion() = %q", v)
	}
}

// The chart creates the key Secret empty and the member may not create
// Secrets: the key is written into that Secret, which keeps its metadata.
func TestKeptKeyFillsTheChartsEmptySecret(t *testing.T) {
	kube := kubefake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fleet-key", Namespace: "box",
		Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"helm.sh/resource-policy": "keep"}}})
	kube.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Error("a Secret was created")
		return true, nil, kerrors.NewForbidden(corev1.Resource("secrets"), "fleet-key", errors.New("no create"))
	})
	key, err := keptKey(context.Background(), kube, "box", "fleet-key", memberTestLog())
	if err != nil {
		t.Fatalf("filling the empty Secret: %v", err)
	}
	s, _ := kube.CoreV1().Secrets("box").Get(context.Background(), "fleet-key", metav1.GetOptions{})
	kept, err := fleet.ParsePrivateKey(s.Data["key"])
	if err != nil || len(s.Data["key.pub"]) == 0 {
		t.Fatalf("the Secret after: %v, %v", s.Data, err)
	}
	if !kept.Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(key.Public()) {
		t.Error("the returned key is not the kept one")
	}
	if s.Labels["app.kubernetes.io/managed-by"] != "Helm" || s.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Errorf("metadata lost: %v %v", s.Labels, s.Annotations)
	}
}

// Two starts filling the empty Secret at once: the one that loses the
// update takes the other's key.
func TestKeptKeyTakesTheKeyFilledMeanwhile(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fleet-key", Namespace: "box"}})
	priv, _, err := fleet.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var updates atomic.Int32
	kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updates.Add(1) > 1 {
			return false, nil, nil
		}
		// The other start wins.
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fleet-key", Namespace: "box"}, Data: map[string][]byte{"key": priv}}
		if err := kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), s, "box"); err != nil {
			t.Fatal(err)
		}
		return true, nil, kerrors.NewConflict(corev1.Resource("secrets"), "fleet-key", errors.New("changed"))
	})
	key, err := keptKey(ctx, kube, "box", "fleet-key", memberTestLog())
	if err != nil {
		t.Fatalf("after a conflict: %v", err)
	}
	want, _ := fleet.ParsePrivateKey(priv)
	if !want.Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(key.Public()) {
		t.Error("not the key the other start kept")
	}
}

// Without the chart's Secrets the member cannot create them: the errors
// say what to do.
func TestFleetSecretsMissingNameTheChart(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(corev1.Resource("secrets"), "x", errors.New("no create"))
	})
	_, err := keptKey(ctx, kube, "box", "fleet-key", memberTestLog())
	if err == nil || !strings.Contains(err.Error(), "box/fleet-key does not exist: the control-plane chart creates it") {
		t.Errorf("keptKey: %v", err)
	}
	err = putSecret(ctx, kube, "box", fleet.SecretGitLab, map[string]string{"token": "t"}, memberTestLog())
	if err == nil || !strings.Contains(err.Error(), "does not exist: the control-plane chart creates it") {
		t.Errorf("putSecret: %v", err)
	}
}

// The hub's requests are RoleHub (not RoleWeb, which may write the store);
// every other request is the authorizer's.
func TestOverLinkAuthorizer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/internal/v1/x", nil)
	asked := false
	authorize := func(*http.Request) (control.Role, error) { asked = true; return control.RoleWeb, nil }
	if role, err := overLinkAuthorizer(func(context.Context) bool { return true }, authorize)(r); err != nil || role != control.RoleHub || asked {
		t.Errorf("over the link: %q, %v, authorize asked %v", role, err, asked)
	}
	if role, err := overLinkAuthorizer(func(context.Context) bool { return false }, authorize)(r); err != nil || role != control.RoleWeb || !asked {
		t.Errorf("not over the link: %q, %v, authorize asked %v", role, err, asked)
	}
}
