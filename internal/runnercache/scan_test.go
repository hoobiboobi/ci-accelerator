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

// everyRule is a lifecycle rule that expires the whole bucket.
const everyRule = `<Rule><ID>x</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>14</Days></Expiration></Rule>`

// fakeS3 answers ListObjectsV2 in two pages and the lifecycle question
// with rules ("" = no lifecycle configuration).
func fakeS3(t *testing.T, rules string) *httptest.Server {
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
		case q.Has("lifecycle") && rules == "":
			w.WriteHeader(404)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
		case q.Has("lifecycle"):
			fmt.Fprint(w, `<?xml version="1.0"?><LifecycleConfiguration>`+rules+`</LifecycleConfiguration>`)
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

var keys = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}, nil
})

func TestScan(t *testing.T) {
	for _, rules := range []string{everyRule, ""} {
		srv := fakeS3(t, rules)
		l := Lister{Store: Store{Server: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, Bucket: "ci-cache", Region: "eu-1", Path: "/box/051b90/"}, Keys: keys}
		s, err := l.Scan(context.Background(), time.Now())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Objects) != 3 || s.Objects[0] != (Object{ProjectID: 42, Key: "go-mod-main-protected", Bytes: 67128, Modified: time.Date(2026, 10, 22, 19, 53, 44, 0, time.UTC)}) ||
			s.Objects[1].Key != "deps/with/slashes" || s.Objects[2].ProjectID != 7 || s.Objects[2].Key != "tools-non_protected" {
			t.Errorf("objects %+v", s.Objects)
		}
		if s.Lifecycle == nil || *s.Lifecycle != (rules != "") {
			t.Errorf("lifecycle %q: %v", rules, s.Lifecycle)
		}
	}
	if got := (Store{Server: "s3.example.com", Bucket: "b", Path: "/x/y/"}).ID(); got != "s3.example.com/b/x/y" {
		t.Errorf("id %q", got)
	}
}

// TestLifecycleCoversTheFolder: only a rule that expires the runners'
// objects counts; a rule for another folder, for tagged or sized objects
// (cache archives have no tags) or only for delete markers leaves the
// caches growing.
func TestLifecycleCoversTheFolder(t *testing.T) {
	rule := func(filter, expiration string) string {
		return `<Rule><ID>r</ID><Status>Enabled</Status>` + filter + `<Expiration>` + expiration + `</Expiration></Rule>`
	}
	days := `<Days>7</Days>`
	for _, c := range []struct {
		name, rules string
		want        bool
	}{
		{"whole bucket", rule(`<Filter><Prefix></Prefix></Filter>`, days), true},
		{"no filter", rule(``, days), true},
		{"a parent folder", rule(`<Filter><Prefix>box/</Prefix></Filter>`, days), true},
		{"the folder itself", rule(`<Filter><Prefix>box/051b90/</Prefix></Filter>`, days), true},
		{"old-style rule prefix", `<Rule><ID>r</ID><Prefix>box/</Prefix><Status>Enabled</Status><Expiration><Days>7</Days></Expiration></Rule>`, true},
		{"a date", rule(`<Filter><Prefix></Prefix></Filter>`, `<Date>2026-12-01T00:00:00Z</Date>`), true},
		{"another folder and a good one", rule(`<Filter><Prefix>logs/</Prefix></Filter>`, days) + everyRule, true},
		{"another folder", rule(`<Filter><Prefix>logs/</Prefix></Filter>`, days), false},
		{"a prefix of the folder's name", rule(`<Filter><Prefix>box/051b9</Prefix></Filter>`, days), true},
		{"a sibling folder", rule(`<Filter><Prefix>box/051b90x/</Prefix></Filter>`, days), false},
		{"a folder inside", rule(`<Filter><Prefix>box/051b90/runner/</Prefix></Filter>`, days), false},
		{"old-style other prefix", `<Rule><ID>r</ID><Prefix>logs/</Prefix><Status>Enabled</Status><Expiration><Days>7</Days></Expiration></Rule>`, false},
		{"a tag", rule(`<Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter>`, days), false},
		{"a tag and a prefix", rule(`<Filter><And><Prefix></Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>`, days), false},
		{"a size", rule(`<Filter><ObjectSizeGreaterThan>1048576</ObjectSizeGreaterThan></Filter>`, days), false},
		{"delete markers only", rule(`<Filter><Prefix></Prefix></Filter>`, `<ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>`), false},
		{"disabled", `<Rule><ID>r</ID><Status>Disabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule>`, false},
	} {
		srv := fakeS3(t, c.rules)
		l := Lister{Store: Store{Server: strings.TrimPrefix(srv.URL, "http://"), Insecure: true, Bucket: "ci-cache", Region: "eu-1", Path: "box/051b90"}, Keys: keys}
		s, err := l.Scan(context.Background(), time.Now())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if s.Lifecycle == nil {
			t.Errorf("%s: lifecycle unknown", c.name)
		} else if *s.Lifecycle != c.want {
			t.Errorf("%s: lifecycle %v, want %v", c.name, *s.Lifecycle, c.want)
		}
	}
}
