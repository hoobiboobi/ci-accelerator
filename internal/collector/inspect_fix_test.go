package collector

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
)

// bigCache answers DiskUsage with more records than 16 MiB hold.
type bigCache struct {
	controlapi.UnimplementedControlServer
	n int
}

func (b bigCache) DiskUsage(context.Context, *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	resp := &controlapi.DiskUsageResponse{}
	desc := "pulled from docker.io/library/" + strings.Repeat("x", 160)
	for i := 0; i < b.n; i++ {
		resp.Record = append(resp.Record, &controlapi.UsageRecord{ID: "sha256:0123456789abcdef0123456789abcdef", Size: 1, RecordType: "regular", Description: desc})
	}
	return resp, nil
}

// A daemon with a very large cache (about 100,000 records, over the 16 MiB
// BuildKit's client accepts in one answer) is still listed.
func TestLargeCacheIsListed(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	controlapi.RegisterControlServer(srv, bigCache{n: 100000})
	go srv.Serve(lis)
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	records, err := diskUsage(ctx, controlapi.NewControlClient(conn))
	if err != nil {
		t.Fatalf("a large cache is not listed: %v", err)
	}
	if len(records) != 100000 || Classify(records).Images != 100000 {
		t.Errorf("%d records", len(records))
	}
}

// A daemon whose cache cannot be listed still gets its disk and load read
// now; the cache's figures stay the last ones read, with the error.
func TestUnlistedCacheKeepsDiskCurrent(t *testing.T) {
	var l live
	l.connect("d1")
	l.inspected("d1", Inspection{Node: "n1", Usage: Usage{Total: 100}, Disk: &diskprobe.Disk{Available: 50}}, nil)
	first := l.daemons["d1"].InspectedAt
	l.inspected("d1", Inspection{Node: "n1", Disk: &diskprobe.Disk{Available: 5}}, &usageError{errors.New("received message larger than max")})
	d := l.daemons["d1"]
	if d.Inspection.Disk == nil || d.Inspection.Disk.Available != 5 {
		t.Errorf("disk %+v, want the reading of now", d.Inspection.Disk)
	}
	if d.Inspection.Usage.Total != 100 || d.InspectErr == "" || !d.InspectedAt.Equal(first) {
		t.Errorf("usage %+v err %q at %v", d.Inspection.Usage, d.InspectErr, d.InspectedAt)
	}
	// Any other failure keeps what was read before.
	l.inspected("d1", Inspection{}, errors.New("daemon down"))
	if d := l.daemons["d1"]; d.Inspection.Disk == nil || d.Inspection.Disk.Available != 5 || d.InspectErr != "daemon down" {
		t.Errorf("%+v", d)
	}
}
