// Package runnercache lists what the runners' S3 cache (GitLab's cache:
// keyword) holds per project, for the console's nodes page.
//
// The runner (19.x) keeps a project's caches under
// <path>/project/<project id>/<cache key> (with Shared = true; a
// runner/<short token>/ folder before project/ otherwise) and never deletes
// them: only a bucket lifecycle rule does.
package runnercache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Store is a bucket folder the runners keep caches in, as the chart
// describes it to the console.
type Store struct {
	Server    string `json:"server"` // host[:port]
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	Insecure  bool   `json:"insecure,omitempty"`
	PathStyle *bool  `json:"pathStyle,omitempty"`
	Path      string `json:"path"` // the runners' Path; "" for the bucket's root
	// Instance is the GitLab (trace-id prefix) whose project ids the folder
	// holds.
	Instance string `json:"instance"`
	// Secret names the Kubernetes Secret with the keys (accesskey,
	// secretkey) the console reads the bucket with.
	Secret string `json:"secret"`
}

// ID names the store in the console's records.
func (s Store) ID() string {
	return strings.TrimSuffix(fmt.Sprintf("%s/%s/%s", s.Server, s.Bucket, strings.Trim(s.Path, "/")), "/")
}

// Object is one cache archive.
type Object struct {
	ProjectID uint64
	Key       string
	Bytes     int64
	Modified  time.Time
}

// Scan is a store's content at one time.
type Scan struct {
	At      time.Time
	Objects []Object
	// Lifecycle: the bucket has a lifecycle rule (old caches expire); nil:
	// the store did not say.
	Lifecycle *bool
}

// MaxObjects bounds one scan; a larger folder is reported as an error
// rather than listed for minutes.
const MaxObjects = 200000

// Lister reads a store with keys.
type Lister struct {
	Store Store
	Keys  aws.CredentialsProvider
	HTTP  *http.Client
}

func (l Lister) client() *s3.Client {
	scheme := "https"
	if l.Store.Insecure {
		scheme = "http"
	}
	// As the runner does: path style unless the store is AWS, or as told.
	pathStyle := !strings.HasSuffix(strings.Split(l.Store.Server, ":")[0], "amazonaws.com")
	if l.Store.PathStyle != nil {
		pathStyle = *l.Store.PathStyle
	}
	hc := l.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return s3.New(s3.Options{
		Region:       l.Store.Region,
		BaseEndpoint: aws.String(scheme + "://" + l.Store.Server),
		UsePathStyle: pathStyle,
		Credentials:  l.Keys,
		HTTPClient:   hc,
		// S3-compatible stores reject the newer checksum headers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// cacheKey splits an object key below the store's folder into the project
// and the cache key: [runner/<token>/]project/<id>/<key>.
var cacheKey = regexp.MustCompile(`^(?:runner/[^/]+/)?project/([0-9]+)/(.+)$`)

// Scan lists the store's caches and asks whether the bucket expires them.
func (l Lister) Scan(ctx context.Context, now time.Time) (Scan, error) {
	c := l.client()
	out := Scan{At: now}
	prefix := strings.Trim(l.Store.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(l.Store.Bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("listing %s: %w", l.Store.ID(), err)
		}
		for _, o := range page.Contents {
			m := cacheKey.FindStringSubmatch(strings.TrimPrefix(aws.ToString(o.Key), prefix))
			if m == nil {
				continue
			}
			id, err := strconv.ParseUint(m[1], 10, 64)
			if err != nil || id == 0 {
				continue
			}
			out.Objects = append(out.Objects, Object{ProjectID: id, Key: m[2], Bytes: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified).UTC()})
			if len(out.Objects) > MaxObjects {
				return out, fmt.Errorf("listing %s: more than %d objects", l.Store.ID(), MaxObjects)
			}
		}
	}
	lc, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(l.Store.Bucket)})
	var api smithy.APIError
	switch {
	case err == nil:
		has := false
		for _, r := range lc.Rules {
			has = has || (r.Status == "Enabled" && r.Expiration != nil)
		}
		out.Lifecycle = &has
	case errors.As(err, &api) && api.ErrorCode() == "NoSuchLifecycleConfiguration":
		no := false
		out.Lifecycle = &no
	}
	// Other errors (no permission to read the rule): unknown, not fatal.
	return out, nil
}
