package fleet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The last five events per name, the newest first; a long text is cut;
// a name a member could not have is not kept; nil keeps nothing.
func TestLinkEventsKeepTheLastFive(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	e := &LinkEvents{now: func() time.Time { at = at.Add(time.Minute); return at }}
	for i := 1; i <= 7; i++ {
		e.Add("eu", fmt.Sprintf("event %d", i))
	}
	e.Add("us", strings.Repeat("é", 400))
	e.Add("../x", "a name no member has")
	got := e.Last("eu")
	if len(got) != LinkEventsKept || got[0].Text != "event 7" || got[4].Text != "event 3" || !got[0].At.After(got[1].At) {
		t.Fatalf("eu: %+v", got)
	}
	if us := e.Last("us"); len(us) != 1 || len(us[0].Text) > linkEventText+len("…") || !strings.HasSuffix(us[0].Text, "…") || !strings.HasPrefix(us[0].Text, "é") {
		t.Errorf("a long text: %q", us)
	}
	if len(e.Last("../x")) != 0 {
		t.Error("an invalid name is kept")
	}
	e.Forget("eu")
	if len(e.Last("eu")) != 0 {
		t.Error("forgotten, still there")
	}
	var none *LinkEvents
	none.Add("eu", "x")
	if none.Last("eu") != nil {
		t.Error("nil keeps events")
	}
}

// Made-up names cannot grow the hub's memory: the name written to longest
// ago goes first.
func TestLinkEventsBoundTheNames(t *testing.T) {
	e := &LinkEvents{}
	e.Add("first", "x")
	for i := 0; i < linkEventNames; i++ {
		e.Add(fmt.Sprintf("n%d", i), "x")
	}
	if len(e.by) != linkEventNames || len(e.order) != linkEventNames {
		t.Fatalf("names kept: %d %d", len(e.by), len(e.order))
	}
	if len(e.Last("first")) != 0 || len(e.Last("n0")) != 1 {
		t.Error("not the oldest name went")
	}
}

// What the hub refuses and the links that close are recorded for the
// cluster's page, with where they came from.
func TestHubRecordsLinkEvents(t *testing.T) {
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	ev := &LinkEvents{}
	p.hub.Events = ev
	// Another protocol.
	q := url.Values{"name": {"eu"}, "store": {testStore}, "version": {"test"}, "protocol": {"99"}}
	resp, err := http.Get(p.srv.URL + ConnectPath + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := ev.Last("eu"); len(got) != 1 || !strings.Contains(got[0].Text, "refused, from 127.0.0.1:") ||
		!strings.Contains(got[0].Text, "it speaks protocol 99; this hub speaks ") {
		t.Fatalf("protocol: %+v", got)
	}
	// A key the hub does not know.
	d, fp := member(t, "eu", p.srv.URL, hubPub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ev.Last("eu")) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if got := ev.Last("eu"); len(got) < 2 || !strings.Contains(got[0].Text, "refused, from ") || !strings.Contains(got[0].Text, fp) {
		t.Fatalf("refused key: %+v", got)
	}
	// A link that comes up and closes (another hub, which knows the key).
	holders2 := map[string]string{}
	p2, hubPub2 := startHub(t, holders2)
	ev2 := &LinkEvents{}
	p2.hub.Events = ev2
	d2, fp2 := member(t, "eu", p2.srv.URL, hubPub2, nil)
	holders2[fp2] = "eu"
	l := up(t, p2, d2)
	if got := ev2.Last("eu"); len(got) != 0 {
		t.Errorf("a link that came up is an event: %+v", got)
	}
	l.Close()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ev2.Last("eu")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := ev2.Last("eu"); len(got) == 0 || !strings.HasPrefix(got[0].Text, "closed after ") || !strings.Contains(got[0].Text, ", from 127.0.0.1:") {
		t.Fatalf("closed: %+v", got)
	}
}
