package runnercache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func listerOf(srv *httptest.Server) Lister {
	return Lister{Store: Store{Server: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, Bucket: "ci-cache", Region: "eu-1", Path: "box"}, Keys: keys}
}

// A server that answers 200 with something else than an S3 listing (a web
// page, an empty body: a proxy or a store's web UI at the address) is an
// error, not an empty cache folder.
func TestScanRefusesAnswersThatAreNoListing(t *testing.T) {
	for name, body := range map[string]string{
		"html":  "<!doctype html><html><body>Sign in</body></html>",
		"empty": "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, body)
		}))
		s, err := listerOf(srv).Scan(context.Background(), time.Now())
		srv.Close()
		if err == nil {
			t.Errorf("%s answer: no error, objects %v lifecycle %v", name, s.Objects, s.Lifecycle)
		}
	}
}

// A lifecycle answer without rules (S3 itself answers a bucket without
// rules with an error) says nothing: the rule is unknown, not missing.
func TestScanLifecycleWithoutRulesIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("lifecycle") {
			return // 200, empty
		}
		fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><Name>ci-cache</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()
	s, err := listerOf(srv).Scan(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Lifecycle != nil {
		t.Errorf("lifecycle %v from an answer without rules, want unknown", *s.Lifecycle)
	}
}

// A redirect is not followed: neither the signed request goes to the other
// address, nor is its answer taken as the store's.
func TestScanDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><Name>ci-cache</Name><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>box/project/9/forged</Key><LastModified>2026-10-22T19:53:44.000Z</LastModified><Size>999</Size></Contents></ListBucketResult>`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s, err := listerOf(srv).Scan(context.Background(), time.Now())
	if err == nil || len(s.Objects) != 0 {
		t.Errorf("objects %+v, err %v: the redirect's answer was taken", s.Objects, err)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect was followed %d times", n)
	}
}

// A folder with more than MaxObjects caches is an error, not listed on.
func TestScanBounded(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("lifecycle") {
			w.WriteHeader(404)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
			return
		}
		page := pages.Add(1)
		var b strings.Builder
		fmt.Fprintf(&b, `<?xml version="1.0"?><ListBucketResult><Name>ci-cache</Name><IsTruncated>true</IsTruncated><NextContinuationToken>p%d</NextContinuationToken>`, page)
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(&b, `<Contents><Key>box/project/1/k%d-%d</Key><LastModified>2026-10-22T19:53:44.000Z</LastModified><Size>1</Size></Contents>`, page, i)
		}
		b.WriteString(`</ListBucketResult>`)
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := listerOf(srv).Scan(ctx, time.Now())
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("err %v after %d pages, want the bound", err, pages.Load())
	}
	if n := pages.Load(); n > MaxObjects/1000+1 {
		t.Errorf("%d pages read", n)
	}
}
