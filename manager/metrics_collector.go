package manager

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/runc/task"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"golang.org/x/exp/maps"
)

func Handler(w http.ResponseWriter, req *http.Request) {
	fetchMetricsAndMerge(w)
}

// fetchMetricsAndMerge gets metrics from each socket, merges them together
// and writes them to w.
func fetchMetricsAndMerge(w io.Writer) {
	socks, err := os.ReadDir(task.ShimSocketPath)
	if err != nil {
		slog.Error("error listing file in shim socket path", "path", task.ShimSocketPath, "err", err)
		return
	}

	mfs := map[string]*dto.MetricFamily{}
	for _, sock := range socks {
		sockName := filepath.Join(task.ShimSocketPath, sock.Name())
		slog.Debug("getting metrics", "name", sockName)

		shimMetrics, err := getMetricsOverTTRPC(context.Background(), sockName)
		if err != nil {
			slog.Error("getting metrics", "err", err)
			// we still want to read the rest of the sockets
			continue
		}
		for _, mf := range shimMetrics {
			if mf.Name == nil {
				continue
			}

			mfo, ok := mfs[*mf.Name]
			if ok {
				mfo.Metric = append(mfo.Metric, mf.Metric...)
			} else {
				mfs[*mf.Name] = mf
			}
		}
	}
	keys := maps.Keys(mfs)
	slices.Sort(keys)
	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, n := range keys {
		err := enc.Encode(mfs[n])
		if err != nil {
			slog.Error("encoding metrics", "err", err)
			return
		}
	}
}

func getMetricsOverTTRPC(ctx context.Context, sock string) ([]*dto.MetricFamily, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}

	resp, err := v1.NewShimClient(ttrpc.NewClient(conn)).Metrics(ctx, &v1.MetricsRequest{})
	if err != nil {
		return nil, err
	}

	return resp.Metrics, nil
}
