package main

import (
	"context"
	"os"
	"time"
)

// urlsEvery is how often a file of GitLab URLs is read again; the kubelet
// takes up to about a minute and a half to update a mounted ConfigMap.
var urlsEvery = 30 * time.Second

// readURLsFile reads the GitLab URLs a file lists (comma-, space- or
// line-separated, as $GITLAB_URLS): its content, to notice a change, and
// the URLs.
func readURLsFile(path string) (string, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return string(b), split(string(b)), nil
}

// followURLsFile calls changed with the new URLs whenever the file at
// path no longer holds was, until ctx ends. The file is a mounted
// ConfigMap, which the kubelet updates in place: an environment variable
// from it would keep its value until the container starts again, and a
// changed URL changes the instance every trace id and job is told by.
func followURLsFile(ctx context.Context, path, was string, every time.Duration, changed func([]string)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, urls, err := readURLsFile(path)
		// A file without URLs keeps the ones in use: no GitLab at all
		// would only stop the controller from starting again.
		if err != nil || now == was || len(urls) == 0 {
			continue
		}
		was = now
		changed(urls)
	}
}
