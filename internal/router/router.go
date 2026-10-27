// Package router is the build router: a TCP proxy in front of the build
// daemons that sends each CI job's connection to the daemon where its
// project's cache is (internal/route decides).
//
// It never terminates TLS. The job's buildctl and the daemon speak mutual
// TLS end to end through it: the daemons' certificate names the router's
// Service, and the job's client certificate is checked by the daemon as
// before. The router holds no key and reads no byte of a build; it knows
// the job by the address its connection comes from.
package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Job is the CI job behind a pod address.
type Job struct {
	Project route.Project
	JobID   uint64
	Node    string
}

// View is what the router watches: the daemons, the job pods and where
// projects are warm.
type View interface {
	// Daemons is every daemon known, ready or not.
	Daemons() []route.Daemon
	// Job is the job whose pod has ip.
	Job(ip string) (Job, bool)
	// Warm is a project's activity by node.
	Warm(p route.Project) []route.Activity
}

// Router accepts the jobs' connections.
type Router struct {
	View   View
	Policy route.Policy
	// Node is where this router runs: a connection from an unknown pod is
	// taken to be from this node.
	Node string
	// Port is the daemons' port.
	Port string
	// Report receives every decision; nil: none.
	Report func(store.Decision)
	Log    *slog.Logger
	// Dial connects to a daemon; nil: TCP with keepalive.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	Now  func() time.Time

	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// keepalive notices a peer that vanished (a node lost without a FIN):
// 15 s idle, then 3 probes 15 s apart.
var keepalive = net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 15 * time.Second, Count: 3}

// Listen opens the router's listener with keepalive on every connection.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	lc := net.ListenConfig{KeepAliveConfig: keepalive}
	return lc.Listen(ctx, "tcp", addr)
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) dial(ctx context.Context, addr string) (net.Conn, error) {
	if r.Dial != nil {
		return r.Dial(ctx, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAliveConfig: keepalive}
	return d.DialContext(ctx, "tcp", addr)
}

// Serve accepts connections until ctx ends, then stops accepting and waits
// up to drain for the open ones to finish (a build in progress finishes
// through the router that started it).
func (r *Router) Serve(ctx context.Context, ln net.Listener, drain time.Duration) error {
	if r.Log == nil {
		r.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		r.track(c, true)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.track(c, false)
			r.handle(ctx, c)
		}()
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	r.Log.Info("draining", "open", r.Open())
	select {
	case <-done:
	case <-time.After(drain):
		r.Log.Warn("drain time over: closing open connections", "open", r.Open())
		r.mu.Lock()
		for c := range r.conns {
			c.Close()
		}
		r.mu.Unlock()
		<-done
	}
	return nil
}

func (r *Router) track(c net.Conn, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = map[net.Conn]struct{}{}
	}
	if open {
		r.conns[c] = struct{}{}
	} else {
		delete(r.conns, c)
	}
}

// Open is the number of connections being served.
func (r *Router) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// Decide routes a connection from ip.
func (r *Router) Decide(ip string) (route.Decision, store.Decision, error) {
	req := route.Request{Node: r.Node}
	job, known := r.View.Job(ip)
	if known {
		req.Project, req.Node = &job.Project, job.Node
	}
	daemons := r.View.Daemons()
	for _, d := range daemons {
		if d.Node == req.Node && d.Zone != "" {
			req.Zone = d.Zone
		}
	}
	var warm []route.Activity
	if known {
		warm = r.View.Warm(job.Project)
	}
	now := r.now()
	d, err := route.Decide(req, daemons, warm, r.Policy, now)
	rec := store.Decision{JobID: job.JobID, Node: d.Daemon.Node, Daemon: d.Daemon.Name, Reason: d.Reason,
		Cands: d.Candidates, JobNode: req.Node, At: now}
	if known {
		rec.Project = job.Project
	}
	return d, rec, err
}

func (r *Router) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	d, rec, err := r.Decide(ip)
	if err != nil {
		r.Log.Warn("connection refused: no daemon", "from", ip, "err", err)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	up, err := r.dial(dctx, net.JoinHostPort(d.Daemon.Addr, r.Port))
	cancel()
	if err != nil {
		r.Log.Warn("daemon not reached", "daemon", d.Daemon.Name, "from", ip, "err", err)
		return
	}
	defer up.Close()
	if r.Report != nil && rec.Reason != route.UnknownLocal && rec.Reason != route.UnknownHash {
		r.Report(rec)
	}
	r.Log.Debug("routed", "from", ip, "job", rec.JobID, "daemon", d.Daemon.Name, "reason", d.Reason)
	pipe(c, up)
}

// pipe copies both ways until both directions end. One side closing its
// sending half is passed on (half-close), so a request can finish while
// the answer still comes back.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
		} else {
			// An error (or a peer that cannot half-close) ends both ways.
			a.Close()
			b.Close()
		}
	}
	go half(b, a)
	go half(a, b)
	wg.Wait()
}
