package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hoobiboobi/ci-accelerator/internal/runnercache"
)

func TestLoadCaches(t *testing.T) {
	dir := t.TempDir()
	write := func(text string) string {
		p := filepath.Join(dir, fmt.Sprintf("c%d.json", len(text)))
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	one := `{"server":"s3.example.com","bucket":"ci-cache","region":"hel1","path":"box/051b90","instance":"051b90","secret":"runner-cache"}`
	got, err := loadCaches(write("[" + one + "," + one + "]"))
	if err != nil || len(got) != 1 || got[0].ID() != "s3.example.com/ci-cache/box/051b90" {
		t.Fatalf("stores %+v, %v", got, err)
	}
	if got, err := loadCaches(write("[]")); err != nil || len(got) != 0 {
		t.Errorf("no store: %+v, %v", got, err)
	}
	for name, text := range map[string]string{
		"no secret":     `[{"server":"s3.example.com","bucket":"ci-cache"}]`,
		"unknown field": `[{"server":"s3.example.com","bucket":"ci-cache","secret":"s","bukket":"x"}]`,
	} {
		if _, err := loadCaches(write(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestScanCache(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Query().Has("lifecycle") {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
			return
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>box/051b90/project/42/go-mod</Key><LastModified>2026-10-22T19:53:44.000Z</LastModified><Size>67128</Size></Contents></ListBucketResult>`)
	}))
	defer srv.Close()
	kube := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runner-cache", Namespace: "ci"},
		Data: map[string][]byte{"accesskey": []byte("AKID\n"), "secretkey": []byte("SECRET")}})
	s := runnercache.Store{Server: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, Bucket: "ci-cache", Region: "hel1", Path: "box/051b90", Secret: "runner-cache"}
	scan, err := scanCache(context.Background(), s, "ci", kube)
	if err != nil || len(scan.Objects) != 1 || scan.Objects[0].Bytes != 67128 || scan.Lifecycle == nil || *scan.Lifecycle {
		t.Fatalf("scan %+v, %v", scan, err)
	}
	if !strings.Contains(auth, "Credential=AKID/") {
		t.Errorf("signed with %q", auth)
	}
	s.Secret = "missing"
	if _, err := scanCache(context.Background(), s, "ci", kube); err == nil || !strings.Contains(err.Error(), "Secret missing") {
		t.Errorf("missing Secret: %v", err)
	}
}
