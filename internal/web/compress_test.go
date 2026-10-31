package web

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// raw asks for path saying it accepts enc ("" for nothing) and returns the
// response as sent, not decompressed.
func raw(t *testing.T, base, path, enc string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	if enc != "" {
		req.Header.Set("Accept-Encoding", enc)
	}
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, b
}

// Pages, the search index and the stylesheet go compressed to a client that
// accepts it, and as they are to one that does not; images and event
// streams never.
func TestCompressedResponses(t *testing.T) {
	ts, _, _ := console(t)
	plain, body := raw(t, ts.URL, "/", "")
	if plain.Header.Get("Content-Encoding") != "" || !strings.Contains(string(body), "<title>Pipelines") {
		t.Fatalf("without Accept-Encoding: %q, %d bytes", plain.Header.Get("Content-Encoding"), len(body))
	}
	res, zipped := raw(t, ts.URL, "/", "gzip")
	if res.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("with gzip: encoding %q, vary %q", res.Header.Get("Content-Encoding"), res.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(strings.NewReader(string(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	unzipped, err := io.ReadAll(zr)
	if err != nil || !strings.Contains(string(unzipped), "<title>Pipelines") || len(zipped)*3 > len(unzipped) {
		t.Errorf("gzip: %d bytes for %d (%v)", len(zipped), len(unzipped), err)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("a compressed page lost its security headers")
	}
	css := regexp.MustCompile(`/static/console\.css\?v=[0-9a-f]+`).FindString(string(body))
	png := regexp.MustCompile(`/static/og\.png\?v=[0-9a-f]+`).FindString(string(body))
	if res, _ := raw(t, ts.URL, css, "gzip, zstd"); res.Header.Get("Content-Encoding") == "" {
		t.Errorf("the stylesheet %s is not compressed", css)
	}
	if res, _ := raw(t, ts.URL, png, "gzip"); res.Header.Get("Content-Encoding") != "" {
		t.Errorf("the image %s is compressed: %s", png, res.Header.Get("Content-Encoding"))
	}
	if res, _ := raw(t, ts.URL, "/search-index", "gzip"); res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("the search index is not compressed")
	}
}

// A live page's event stream starts at once, uncompressed, however much
// the client accepts compression: a compressor would hold events back.
func TestEventsAreNotCompressed(t *testing.T) {
	ts, _, _ := console(t)
	req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatalf("the event stream is compressed: %s", res.Header.Get("Content-Encoding"))
	}
	line := make(chan string, 1)
	go func() { l, _ := bufio.NewReader(res.Body).ReadString('\n'); line <- l }()
	select {
	case l := <-line:
		if l != "retry: 5000\n" {
			t.Errorf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Error("the stream's first line did not arrive")
	}
}
