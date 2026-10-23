package runnercache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeS3 answers ListObjectsV2 in two pages and the lifecycle question.
func fakeS3(t *testing.T, lifecycle int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
			w.WriteHeader(403)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path != "/ci-cache" && r.URL.Path != "/ci-cache/":
			t.Errorf("path %s (path style expected)", r.URL.Path)
			w.WriteHeader(404)
		case q.Has("lifecycle") && lifecycle == 404:
			w.WriteHeader(404)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
		case q.Has("lifecycle"):
			fmt.Fprint(w, `<?xml version="1.0"?><LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>14</Days></Expiration></Rule></LifecycleConfiguration>`)
		case q.Get("list-type") == "2" && q.Get("prefix") == "box/051b90/":
			obj := func(key string, size int) string {
				return fmt.Sprintf(`<Contents><Key>%s</Key><LastModified>2026-10-22T19:53:44.000Z</LastModified><Size>%d</Size></Contents>`, key, size)
			}
			if q.Get("continuation-token") == "" {
				fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><Name>ci-cache</Name><IsTruncated>true</IsTruncated><NextContinuationToken>p2</NextContinuationToken>`+
					obj("box/051b90/project/42/go-mod-main-protected", 67128)+obj("box/051b90/project/42/deps/with/slashes", 10)+obj("box/051b90/other/file", 5)+`</ListBucketResult>`)
				return
			}
			fmt.Fprint(w, `<?xml version="1.0"?><ListBucketResult><Name>ci-cache</Name><IsTruncated>false</IsTruncated>`+
				obj("box/051b90/runner/abc123/project/7/tools-non_protected", 353)+obj("box/051b90/project/0/bad", 1)+`</ListBucketResult>`)
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(400)
		}
	}))
}

func TestScan(t *testing.T) {
	keys := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}, nil
	})
	for _, lc := range []int{200, 404} {
		srv := fakeS3(t, lc)
		l := Lister{Store: Store{Server: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, Bucket: "ci-cache", Region: "hel1", Path: "/box/051b90/"}, Keys: keys}
		s, err := l.Scan(context.Background(), time.Now())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Objects) != 3 || s.Objects[0] != (Object{ProjectID: 42, Key: "go-mod-main-protected", Bytes: 67128, Modified: time.Date(2026, 10, 22, 19, 53, 44, 0, time.UTC)}) ||
			s.Objects[1].Key != "deps/with/slashes" || s.Objects[2].ProjectID != 7 || s.Objects[2].Key != "tools-non_protected" {
			t.Errorf("objects %+v", s.Objects)
		}
		if s.Lifecycle == nil || *s.Lifecycle != (lc == 200) {
			t.Errorf("lifecycle %d: %v", lc, s.Lifecycle)
		}
	}
	if got := (Store{Server: "s3.example.com", Bucket: "b", Path: "/x/y/"}).ID(); got != "s3.example.com/b/x/y" {
		t.Errorf("id %q", got)
	}
}
