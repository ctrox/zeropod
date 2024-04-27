package manager

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/ctrox/zeropod/zeropod"
	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/common/expfmt"
)

func Handler(w http.ResponseWriter, req *http.Request) {
	fetchMetricsAndMerge(w)
}

// fetchMetricsAndMerge gets metrics from each socket, merges them together
// and writes them to w.
func fetchMetricsAndMerge(w io.Writer) {
	socks, err := os.ReadDir(zeropod.MetricsSocketPath)
	if err != nil {
		slog.Error("error listing file in metrics socket path", "path", zeropod.MetricsSocketPath, "err", err)
		return
	}

	mfs := map[string]*dto.MetricFamily{}
	for _, sock := range socks {
		sockName := filepath.Join(zeropod.MetricsSocketPath, sock.Name())
		slog.Info("reading sock", "name", sockName)

		res, err := getMetrics(sockName)
		if err != nil {
			slog.Error("getting metrics", "err", err)
			// we still want to read the rest of the sockets
			continue
		}

		for n, mf := range res {
			mfo, ok := mfs[n]
			if ok {
				mfo.Metric = append(mfo.Metric, mf.Metric...)
			} else {
				mfs[n] = mf
			}
		}
	}

	names := []string{}
	for n := range mfs {
		names = append(names, n)
	}
	sort.Strings(names)

	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, n := range names {
		err := enc.Encode(mfs[n])
		if err != nil {
			slog.Error("encoding metrics", "err", err)
			return
		}
	}
}

func getMetrics(sock string) (map[string]*dto.MetricFamily, error) {
	tr := &http.Transport{
		Dial: func(proto, addr string) (conn net.Conn, err error) {
			return net.Dial("unix", sock)
		},
	}

	client := &http.Client{Transport: tr}

	// the host does not seem to matter when using unix sockets
	resp, err := client.Get("http://localhost/metrics")
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expected status 200, got %v", resp.StatusCode)
	}

	var parser expfmt.TextParser
	mfs, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, err
	}

	return mfs, nil
}
