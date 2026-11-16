package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// fleetDialer checks a member's settings and makes its link to the hub,
// without its key: runMember takes the key and trades a join token in the
// background, so the box's own work never waits for the hub.
func fleetDialer(c config, st *store.Store, h http.Handler, log *slog.Logger) (*fleet.Dialer, *tls.Config, error) {
	if err := hubAddress(c.fleetHub, c.fleetJoin != ""); err != nil {
		return nil, nil, err
	}
	hb, err := os.ReadFile(c.fleetHubKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("-fleet-hub-keys-file: %w", err)
	}
	hubKeys, err := fleet.ParsePublicKeys(hb)
	if err != nil {
		return nil, nil, fmt.Errorf("-fleet-hub-keys-file: %w", err)
	}
	var outer *tls.Config
	if c.fleetHubCA != "" {
		b, err := os.ReadFile(c.fleetHubCA)
		if err != nil {
			return nil, nil, fmt.Errorf("-fleet-hub-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, nil, errors.New("-fleet-hub-ca-file: no PEM certificate in it")
		}
		outer = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	// A key file that is wrong is the operator's to fix: said at start.
	if _, err := fileKey(c); err != nil {
		return nil, nil, err
	}
	log.Info("fleet: this box is a member", "hub", c.fleetHub, "name", c.fleetName, "hub_keys", len(hubKeys), "join_token", c.fleetJoin != "")
	return &fleet.Dialer{Hub: c.fleetHub, Name: c.fleetName, Store: storeID(st), Version: versionString(), HubKeys: hubKeys,
		Outer: outer, Handler: h, Log: log}, outer, nil
}

// endSessions ends the sessions of the box's own sign-in once it is a
// member: people sign in at the hub, and a box that leaves the fleet must
// not take the old cookies again.
func endSessions(ctx context.Context, st *store.Store, log *slog.Logger) error {
	n, err := st.DeleteSessions(ctx)
	if err != nil {
		return fmt.Errorf("ending the box's own sign-in sessions: %w", err)
	}
	if n > 0 {
		log.Info("fleet: the box's own sign-in sessions ended", "sessions", n)
	}
	return nil
}

// runMember takes the member's key (and trades its join token), then keeps
// d connected until ctx ends. What fails meanwhile (the Kubernetes API, a
// damaged key Secret, the hub away) is logged and tried again: the box
// collects its builds all the same.
func runMember(ctx context.Context, d *fleet.Dialer, c config, kube kubernetes.Interface, outer *tls.Config, log *slog.Logger) {
	for attempt := 0; ; attempt++ {
		key, err := memberKey(ctx, c, kube, outer, log)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			log.Info("fleet: the member's key", "key", fleet.Fingerprint(key.Public()))
			d.Key = key
			d.Run(ctx)
			return
		}
		wait := joinWait(attempt, false)
		log.Error("fleet: the member's key: not connecting to the hub until it is there", "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// joinWait is how long a member waits before it asks again after attempt
// failures in a row: with jitter, longer each time, up to 5 minutes when
// the hub refused, 30 s otherwise. A variable for the tests.
var joinWait = func(attempt int, refused bool) time.Duration {
	limit := 30 * time.Second
	if refused {
		limit = 5 * time.Minute
	}
	b := time.Second << min(attempt, 9)
	if b > limit {
		b = limit
	}
	return b/2 + rand.N(b/2+1)
}

// fileKey is the key in -fleet-key-file; nil when it is empty or (with a
// join token) not there.
func fileKey(c config) (crypto.Signer, error) {
	kb, err := os.ReadFile(c.fleetKey)
	if err != nil && (c.fleetJoin == "" || !errors.Is(err, os.ErrNotExist)) {
		return nil, fmt.Errorf("-fleet-key-file: %w", err)
	}
	if len(bytes.TrimSpace(kb)) == 0 {
		if c.fleetJoin == "" {
			return nil, errors.New("-fleet-key-file holds no key, and no join token is given")
		}
		return nil, nil
	}
	key, err := fleet.ParsePrivateKey(kb)
	if err != nil {
		return nil, fmt.Errorf("-fleet-key-file: %w", err)
	}
	return key, nil
}

// memberKey is the member's private key: from -fleet-key-file; else, with
// a join token, the one kept in -fleet-key-secret, made and kept there
// first if there is none. With a join token the member then trades it
// for its registration, at every start while the token is there, and
// asks again until the hub answers (the token file is read each time: a
// new token put in its Secret counts). The hub answers a key it knows
// under this name as done, so a restart between keeping the key and
// trading the token loses nothing.
func memberKey(ctx context.Context, c config, kube kubernetes.Interface, outer *tls.Config, log *slog.Logger) (crypto.Signer, error) {
	key, err := fileKey(c)
	if err != nil {
		return nil, err
	}
	if c.fleetJoin == "" {
		return key, nil
	}
	if key == nil {
		if key, err = keptKey(ctx, kube, c.namespace, c.fleetKeySecret, log); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		tb, err := os.ReadFile(c.fleetJoin)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("-fleet-join-file: %w", err)
		}
		token := strings.TrimSpace(string(tb))
		if token == "" {
			return key, nil
		}
		err = joinHub(ctx, c, outer, key, token)
		if err == nil {
			log.Info("fleet: joined the hub with a join token", "hub", c.fleetHub, "name", c.fleetName)
			return key, nil
		}
		var refused joinRefused
		wait := joinWait(attempt, errors.As(err, &refused))
		log.Warn("fleet: trading the join token", "hub", c.fleetHub, "name", c.fleetName, "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// keptKey reads the member's key from its Secret, making and keeping one
// there when there is none. The chart creates that Secret empty (the
// controller may not create Secrets): the key is written into it.
func keptKey(ctx context.Context, kube kubernetes.Interface, ns, name string, log *slog.Logger) (crypto.Signer, error) {
	secrets := kube.CoreV1().Secrets(ns)
	for range 3 {
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err == nil && len(s.Data["key"]) > 0 {
			key, err := fleet.ParsePrivateKey(s.Data["key"])
			if err != nil {
				return nil, fmt.Errorf("the fleet key Secret %s/%s, key \"key\": %w", ns, name, err)
			}
			return key, nil
		}
		if err != nil && !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("reading the fleet key Secret %s/%s: %w", ns, name, err)
		}
		priv, pub, gerr := fleet.GenerateKey()
		if gerr != nil {
			return nil, gerr
		}
		if err == nil {
			// The chart's empty Secret: fill it, keeping its labels and
			// annotations.
			s.Data = map[string][]byte{"key": priv, "key.pub": pub}
			_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
			if kerrors.IsConflict(err) {
				continue // another start filled it meanwhile: read that one
			}
			if err != nil {
				return nil, fmt.Errorf("keeping the fleet key in Secret %s/%s: %w", ns, name, err)
			}
			log.Info("fleet: made this member's key and kept it", "secret", ns+"/"+name)
			return fleet.ParsePrivateKey(priv)
		}
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "ci-accelerator"},
				Annotations: map[string]string{"ci-accelerator.io/note": "the fleet member's private key, made by the controller when it joined with a join token"}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"key": priv, "key.pub": pub},
		}, metav1.CreateOptions{})
		if kerrors.IsAlreadyExists(err) {
			continue // another start made it meanwhile: read that one
		}
		if err != nil {
			return nil, missingSecret(ns, name, err)
		}
		log.Info("fleet: made this member's key and kept it", "secret", ns+"/"+name)
		return fleet.ParsePrivateKey(priv)
	}
	return nil, fmt.Errorf("the fleet key Secret %s/%s: made elsewhere but holds no key", ns, name)
}

// missingSecret explains a refused Create of a fleet Secret: the member's
// Role may not create Secrets, the control-plane chart makes them.
func missingSecret(ns, name string, err error) error {
	if kerrors.IsForbidden(err) {
		return fmt.Errorf("the fleet key Secret %s/%s does not exist: the control-plane chart creates it (upgrade the release): %w", ns, name, err)
	}
	return fmt.Errorf("keeping the fleet key in Secret %s/%s: %w", ns, name, err)
}

// hubAddress checks -fleet-hub: an http(s) URL; with a join token, https,
// or http only to this host or to a Service of the cluster (the token
// is not protected by the link's pinned keys, only by the address's TLS).
func hubAddress(hub string, token bool) error {
	u, err := url.Parse(hub)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("-fleet-hub %q: give the hub's console address (https://...)", hub)
	}
	if u.Scheme == "https" || !token {
		return nil
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".svc") || strings.Contains(h, ".svc.") {
		return nil
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-fleet-hub %q: a join token is sent only over https (http only to localhost or a Service of the cluster, *.svc)", hub)
}

// joinRefused is the hub's no to a join token: asking again only helps
// with a new token.
type joinRefused struct{ error }

// joinHub trades a join token for this member's registration. Redirects
// are not followed (the token goes to the address given, nowhere else),
// and only the hub's answer naming this member and its key counts.
func joinHub(ctx context.Context, c config, outer *tls.Config, key crypto.Signer, token string) error {
	if err := hubAddress(c.fleetHub, true); err != nil {
		return err
	}
	pub, err := fleet.PublicPEM(key.Public())
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"name": c.fleetName, "token": token, "public_key": string(pub)})
	u := strings.TrimRight(c.fleetHub, "/") + fleet.JoinPath
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = outer
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(jctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("the hub's address answered %s, to %q: give the hub's own address in -fleet-hub (the token is not sent on)", resp.Status, resp.Header.Get("Location"))
	}
	if resp.StatusCode == http.StatusOK {
		var m struct{ Name, Fingerprint string }
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
			return fmt.Errorf("the answer at %s is not the hub's: %w", u, err)
		}
		if m.Name != c.fleetName || m.Fingerprint != fleet.Fingerprint(key.Public()) {
			return fmt.Errorf("the answer at %s is not the hub's: it names %q with key %s", u, m.Name, m.Fingerprint)
		}
		return nil
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	err = fmt.Errorf("the hub answered %s: %s", resp.Status, e.Error.Message)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return joinRefused{err}
	}
	return err
}

// storeID names a store without saying anything about its key.
func storeID(st *store.Store) string {
	sum := sha256.Sum256(append([]byte("ci-accelerator store id\n"), st.SecretKey()...))
	return hex.EncodeToString(sum[:6])
}

// fleetKeygen writes a key pair for a fleet member or hub. It never
// writes over a file: a hub's key is pinned by every member.
func fleetKeygen(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ci-accelerator fleet-keygen <file> (writes <file>, the private key, and <file>.pub)")
	}
	for _, f := range []string{args[0], args[0] + ".pub"} {
		if _, err := os.Lstat(f); err == nil {
			return fmt.Errorf("%s exists: give a new file (a key in use is pinned by the members)", f)
		}
	}
	priv, pub, err := fleet.GenerateKey()
	if err != nil {
		return err
	}
	if err := writeNew(args[0], priv, 0o600); err != nil {
		return err
	}
	if err := writeNew(args[0]+".pub", pub, 0o644); err != nil {
		return err
	}
	keys, _ := fleet.ParsePublicKeys(pub)
	fmt.Printf("%s and %s.pub written; key %s\n", args[0], args[0], fleet.Fingerprint(keys[0]))
	return nil
}

// writeNew writes a file that must not exist yet, with mode perm.
func writeNew(name string, b []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SecretsPath is where a member takes the Secrets its hub delivers (over
// the link only).
const SecretsPath = "/fleet/v1/secrets"

// deliveredKeys are the keys a hub may write in each Secret it delivers:
// the ones the generated values read. Every value must be non-empty but
// the runner chart's registration token, mounted even when empty.
var deliveredKeys = map[string]map[string]bool{
	fleet.SecretRunner: {"runner-token": true, "runner-registration-token": true},
	fleet.SecretGitLab: {"token": true},
	fleet.SecretS3:     {"accesskey": true, "secretkey": true},
}

// checkDelivery says what is wrong with a delivery, before anything is
// written: a name or key the hub may not write, or an empty value.
func checkDelivery(d fleet.Delivery) (status int, err error) {
	for name, data := range d {
		keys, ok := deliveredKeys[name]
		if !ok || !slices.Contains(fleet.DeliveredSecrets, name) {
			return http.StatusForbidden, fmt.Errorf("the hub may not write the Secret %s", name)
		}
		if len(data) == 0 {
			return http.StatusBadRequest, fmt.Errorf("the Secret %s: no keys", name)
		}
		for k, v := range data {
			if !keys[k] {
				return http.StatusBadRequest, fmt.Errorf("the Secret %s: the hub may not write the key %q", name, k)
			}
			if v == "" && k != "runner-registration-token" {
				return http.StatusBadRequest, fmt.Errorf("the Secret %s: the key %q is empty", name, k)
			}
		}
	}
	return http.StatusOK, nil
}

// secretWriter writes the Secrets the hub delivers into the member's
// namespace: only the names and keys the generated values refer to, and
// only for requests over says are the hub's (fleet.Over: over the link).
// The whole delivery is checked before the first write; when a
// write fails, the answer still says which Secrets were written.
func secretWriter(kube kubernetes.Interface, ns string, over func(context.Context) bool, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !over(r.Context()) {
			http.Error(w, "only the hub, over the link", http.StatusForbidden)
			return
		}
		var in struct {
			Secrets fleet.Delivery `json:"secrets"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, "a JSON object with secrets: "+err.Error(), http.StatusBadRequest)
			return
		}
		if code, err := checkDelivery(in.Secrets); err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		names := slices.Sorted(maps.Keys(in.Secrets))
		written := []string{}
		answer := func(code int, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			out := map[string]any{"written": written}
			if err != nil {
				out["error"] = err.Error()
			}
			json.NewEncoder(w).Encode(out)
		}
		for _, name := range names {
			if err := putSecret(r.Context(), kube, ns, name, in.Secrets[name], log); err != nil {
				log.Error("fleet: writing a Secret the hub delivered", "secret", ns+"/"+name, "written", written, "err", err)
				answer(http.StatusInternalServerError, fmt.Errorf("writing %s: %w", name, err))
				return
			}
			written = append(written, name)
		}
		log.Info("fleet: wrote the Secrets the hub delivered", "namespace", ns, "secrets", written)
		answer(http.StatusOK, nil)
	})
}

// putSecret creates a Secret, or sets the delivered keys in it (other keys
// stay), again when someone else changed it meanwhile.
func putSecret(ctx context.Context, kube kubernetes.Interface, ns, name string, data map[string]string, log *slog.Logger) error {
	secrets := kube.CoreV1().Secrets(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			b := map[string][]byte{}
			for k, v := range data {
				b[k] = []byte(v)
			}
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "ci-accelerator"},
					Annotations: map[string]string{"ci-accelerator.io/note": "delivered by the fleet hub: change it there"}},
				Type: corev1.SecretTypeOpaque, Data: b}, metav1.CreateOptions{})
			if kerrors.IsAlreadyExists(err) {
				// Made meanwhile: update that one.
				return kerrors.NewConflict(corev1.Resource("secrets"), name, err)
			}
			if kerrors.IsForbidden(err) {
				return fmt.Errorf("the Secret %s/%s does not exist: the control-plane chart creates it (upgrade the release): %w", ns, name, err)
			}
			return err
		}
		if err != nil {
			return err
		}
		// The controller's own, or the chart's (it carries the note).
		if s.Labels["app.kubernetes.io/managed-by"] != "ci-accelerator" && s.Annotations["ci-accelerator.io/note"] == "" {
			log.Warn("fleet: the hub sets keys of a Secret it did not make", "secret", ns+"/"+name, "keys", slices.Sorted(maps.Keys(data)))
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		return err
	})
}
