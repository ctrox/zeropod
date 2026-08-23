package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/ctrox/zeropod/activator"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
)

// netinfo has a single purpose: it extracts the [activator.Listeners] from a
// criu snapshot for when we migrate from an older version where the snapshot
// did not contain a zeropod_listeners.json. This is done in a saparate binary
// as importing the crit grpc defs would balloon the binary and memory usage of
// the shim.
func main() {
	containerID := flag.String("id", "", "target container id")
	flag.Parse()
	listeners, err := getListenersFromImage(*containerID)
	if err != nil {
		slog.Error("getting listeners", "error", err)
		os.Exit(1)
	}
	if err := storeListeners(*containerID, listeners); err != nil {
		slog.Error("storing listeners", "error", err)
		os.Exit(1)
	}
	slog.Info("stored listeners", "container_id", *containerID, "listeners", listeners)
}

func storeListeners(containerID string, listeners activator.Listeners) error {
	f, err := os.Create(nodev1.ListenersFile(containerID))
	if err != nil {
		return err
	}
	//nolint:errcheck
	defer f.Close()
	return json.NewEncoder(f).Encode(listeners)
}

func getListenersFromImage(containerID string) (activator.Listeners, error) {
	inetImgPath := filepath.Join(nodev1.SnapshotPath(containerID), "files.img")
	f, err := os.Open(inetImgPath)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck
	defer f.Close()

	cr := crit.New(f, nil, "", false, false)
	img, err := cr.Decode(&fdinfo.FileEntry{})
	if err != nil {
		return nil, fmt.Errorf("failed to decode img: %w", err)
	}

	var sockets activator.Listeners

	for _, entry := range img.Entries {
		fe := entry.Message.(*fdinfo.FileEntry)
		isk := fe.GetIsk()
		if isk == nil {
			continue
		}

		const stateTCPListen = 10
		if isk.GetState() != stateTCPListen {
			continue
		}

		var network activator.Network
		switch isk.GetFamily() {
		case syscall.AF_INET:
			network = activator.NetworkTCP4
		case syscall.AF_INET6:
			if isk.GetV6Only() {
				network = activator.NetworkTCP6ONLY
			} else {
				network = activator.NetworkTCPAny
			}
		default:
			continue
		}

		sockets = append(sockets, activator.Listener{
			Port:    uint16(isk.GetSrcPort()),
			Network: network,
		})
	}
	return sockets, nil
}
