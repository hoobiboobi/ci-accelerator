// Package diskprobe reports the space of a filesystem: the daemon pod's
// sidecar serves it for the cache's directory, because BuildKit's API says
// how much the cache holds but not how much room is left on its disk.
package diskprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"
)

// Disk is a filesystem's size, in bytes and inodes. Free counts the blocks
// reserved for root too (what BuildKit's minFreeSpace compares with);
// Available leaves them out (what the kubelet's eviction thresholds use).
type Disk struct {
	Total      uint64 `json:"totalBytes"`
	Free       uint64 `json:"freeBytes"`
	Available  uint64 `json:"availableBytes"`
	Inodes     uint64 `json:"inodes"`
	InodesFree uint64 `json:"inodesFree"`
}

// Stat reads the filesystem that holds dir.
func Stat(dir string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Bsize)
	return Disk{Total: st.Blocks * bs, Free: st.Bfree * bs, Available: st.Bavail * bs,
		Inodes: st.Files, InodesFree: st.Ffree}, nil
}

// Handler serves GET /disk (the Disk of dir as JSON) and GET /healthz.
func Handler(dir string, stat func(string) (Disk, error)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /disk", func(w http.ResponseWriter, r *http.Request) {
		d, err := stat(dir)
		if err != nil {
			http.Error(w, "statfs failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

// Serve answers on addr until ctx ends.
func Serve(ctx context.Context, addr, dir string) error {
	if _, err := Stat(dir); err != nil {
		return fmt.Errorf("disk probe: %w", err)
	}
	srv := &http.Server{Addr: addr, Handler: Handler(dir, Stat), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Fetch asks a probe (http://host:port) for its Disk.
func Fetch(ctx context.Context, client *http.Client, base string) (Disk, error) {
	var d Disk
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/disk", nil)
	if err != nil {
		return d, err
	}
	res, err := client.Do(req)
	if err != nil {
		return d, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return d, fmt.Errorf("disk probe: %s", res.Status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<10)).Decode(&d); err != nil {
		return d, fmt.Errorf("disk probe: %w", err)
	}
	if d.Total == 0 || d.Free > d.Total || d.Available > d.Free {
		return Disk{}, fmt.Errorf("disk probe: implausible numbers %+v", d)
	}
	return d, nil
}
