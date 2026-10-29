package diskprobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatReadsTheFilesystem(t *testing.T) {
	d, err := Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Free > d.Total || d.Available > d.Free || d.Inodes == 0 {
		t.Errorf("implausible: %+v", d)
	}
}

func TestServeAndFetch(t *testing.T) {
	want := Disk{Total: 100 << 30, Free: 40 << 30, Available: 35 << 30, Inodes: 1000, InodesFree: 400}
	srv := httptest.NewServer(Handler("/x", func(dir string) (Disk, error) {
		if dir != "/x" {
			t.Errorf("statfs of %q", dir)
		}
		return want, nil
	}, nil))
	defer srv.Close()
	got, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil || got != want {
		t.Fatalf("fetch: %+v, %v", got, err)
	}
	if res, err := srv.Client().Get(srv.URL + "/healthz"); err != nil || res.StatusCode != 200 {
		t.Errorf("healthz: %v %v", res, err)
	}
	if res, _ := srv.Client().Post(srv.URL+"/disk", "text/plain", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /disk: %d", res.StatusCode)
	}
}

func TestFetchRefusesNonsense(t *testing.T) {
	for _, d := range []Disk{{}, {Total: 10, Free: 20}, {Total: 10, Free: 5, Available: 6}} {
		srv := httptest.NewServer(Handler("/", func(string) (Disk, error) { return d, nil }, nil))
		if _, err := Fetch(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Errorf("%+v accepted", d)
		}
		srv.Close()
	}
	srv := httptest.NewServer(Handler("/", func(string) (Disk, error) { return Disk{}, errors.New("eio") }, nil))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("a failing statfs accepted")
	}
}
