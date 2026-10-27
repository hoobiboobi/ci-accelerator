package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"

	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// TLS names the client certificate files the collector presents to the
// daemons (cert-manager's names), and the name their certificate must have.
type TLS struct {
	CA, Cert, Key string
	ServerName    string
}

// Daemon is a build daemon reached over mutual TLS.
type Daemon struct {
	name string
	c    *client.Client
	addr string
	// probe is the base URL of the disk probe next to the daemon; empty:
	// none.
	probe string
}

// Dial connects to the daemon at addr (tcp://host:port). Its name is the
// host name the daemon reports, which is its pod's name.
func Dial(ctx context.Context, addr string, tls TLS) (*Daemon, error) {
	c, err := client.New(ctx, addr, client.WithServerConfig(tls.ServerName, tls.CA), client.WithCredentials(tls.Cert, tls.Key))
	if err != nil {
		return nil, err
	}
	workers, err := c.ListWorkers(ctx)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	name := ""
	for _, w := range workers {
		if h := w.Labels["org.mobyproject.buildkit.worker.hostname"]; h != "" {
			name = h
			break
		}
	}
	if name == "" {
		c.Close()
		return nil, fmt.Errorf("%s: the daemon reports no host name", addr)
	}
	return &Daemon{name: name, c: c, addr: addr}, nil
}

// DiskProbe makes Inspect also read the free space of the daemon's cache
// from the probe on port of the daemon's pod (the buildkit chart's
// diskProbe sidecar).
func (d *Daemon) DiskProbe(port int) {
	u, err := url.Parse(d.addr)
	if err != nil || u.Hostname() == "" || port <= 0 {
		return
	}
	d.probe = "http://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
}

// probeClient reads disk probes: plain HTTP inside the cluster, numbers
// only.
var probeClient = &http.Client{Timeout: 3 * time.Second}

// Name is the daemon's pod name.
func (d *Daemon) Name() string { return d.name }

// Close closes the connection.
func (d *Daemon) Close() error { return d.c.Close() }

// Listen streams the daemon's history: every record it keeps, then events
// as they happen.
func (d *Daemon) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	stream, err := d.c.ControlClient().ListenBuildHistory(ctx, &controlapi.BuildHistoryRequest{})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ev.Record == nil {
			continue
		}
		if err := fn(ev.Type, ev.Record); err != nil {
			return err
		}
	}
}

// Blob reads a record's blob through the daemon's content API.
func (d *Daemon) Blob(ctx context.Context, desc *controlapi.Descriptor) ([]byte, error) {
	dg, err := digest.Parse(desc.Digest)
	if err != nil {
		return nil, err
	}
	store := proxy.NewContentStore(d.c.ContentClient())
	return content.ReadBlob(ctx, store, ocispecs.Descriptor{MediaType: desc.MediaType, Digest: dg, Size: desc.Size})
}

// NodeLabel is the worker label carrying the Kubernetes node a daemon runs
// on; the buildkit chart sets it from the downward API.
const NodeLabel = "ci-accelerator.io/node"

// Inspect describes the daemon: its node, version, cache policies and what
// its cache holds.
func (d *Daemon) Inspect(ctx context.Context) (Inspection, error) {
	var in Inspection
	workers, err := d.c.ListWorkers(ctx)
	if err != nil {
		return in, err
	}
	for _, w := range workers {
		if in.Node == "" {
			in.Node = w.Labels[NodeLabel]
		}
		if in.Version == "" {
			in.Version = w.BuildkitVersion.Version
		}
		for _, p := range w.GCPolicy {
			in.GC = append(in.GC, GCRule{Filters: p.Filter, All: p.All, Keep: p.KeepDuration,
				Reserved: p.ReservedSpace, MaxUsed: p.MaxUsedSpace, MinFree: p.MinFreeSpace})
		}
	}
	du, err := d.c.DiskUsage(ctx)
	if err != nil {
		return in, err
	}
	records := make([]Record, len(du))
	for i, r := range du {
		records[i] = Record{Type: string(r.RecordType), Description: r.Description, Size: r.Size, LastUsed: r.LastUsedAt}
	}
	in.Usage = Classify(records)
	if d.probe != "" {
		// The daemon answered: a probe that does not is noted, not fatal.
		if disk, err := diskprobe.Fetch(ctx, probeClient, d.probe); err != nil {
			in.DiskErr = err.Error()
		} else {
			in.Disk = &disk
		}
	}
	return in, nil
}

// Record is one record of a daemon's cache.
type Record struct {
	Type, Description string
	Size              int64
	LastUsed          *time.Time
}

// Classify sums a daemon's cache records by kind: base images (layers
// pulled from a registry), build layers, cache mounts, build contexts, and
// the rest.
func Classify(records []Record) Usage {
	u := Usage{ImageSizes: map[string]int64{}}
	for _, r := range records {
		u.Total += r.Size
		switch {
		case r.Type == "exec.cachemount":
			u.Mounts += r.Size
			m := Mount{Description: r.Description, Size: r.Size}
			if r.LastUsed != nil {
				m.LastUsed = *r.LastUsed
			}
			u.MountList = append(u.MountList, m)
		case r.Type == "source.local":
			u.Contexts += r.Size
		case r.Type == "regular" && strings.HasPrefix(r.Description, "pulled from "):
			u.Images += r.Size
			u.ImageSizes[imageName(strings.TrimPrefix(r.Description, "pulled from "))] += r.Size
		case r.Type == "regular":
			u.Layers += r.Size
		default:
			u.Other += r.Size
		}
	}
	sort.Slice(u.MountList, func(i, j int) bool { return u.MountList[i].Size > u.MountList[j].Size })
	return u
}

// imageName shortens "docker.io/library/python:3.13-slim@sha256:..." to
// "python:3.13-slim".
func imageName(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "docker.io/library/")
	return strings.TrimPrefix(ref, "docker.io/")
}
