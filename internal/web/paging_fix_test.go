package web

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Builds that started in the same millisecond as a page's last build are
// on the Older page: the list pages by start time and id.
func TestBuildListOlderKeepsSameMillisecond(t *testing.T) {
	ts, st := emptyConsole(t, testOpts{})
	at := testNow.Add(-time.Hour)
	for i := 0; i < pageSize+3; i++ {
		savePipeline(t, st, uint64(800+i), at)
	}
	first := get(t, ts, "/builds")
	m := regexp.MustCompile(`href="(/builds\?[^"]*before=[^"]*)"`).FindStringSubmatch(first.body)
	if m == nil {
		t.Fatal("no Older link")
	}
	next := get(t, ts, html.UnescapeString(m[1]))
	if n := strings.Count(first.body, `class="brow`) + strings.Count(next.body, `class="brow`); n != pageSize+3 {
		t.Errorf("%d builds on the two pages, want %d", n, pageSize+3)
	}
}
