package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// getLogPath gets the log path of the container by searching for the last log
// file in the CRI pod log path.
// TODO: it would be nicer to get this path via annotations but it looks like
// containerd only passes that to the sandbox container (pause). One possible
// solution would be to implement log restoring in the sandbox container
// instead of the zeropod.
func getLogPath(cfg *Config) (string, error) {
	logDir := fmt.Sprintf("/var/log/pods/%s_%s_%s/%s", cfg.PodNamespace, cfg.PodName, cfg.PodUID, cfg.ContainerName)

	dir, err := os.Open(logDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()

	names, err := dir.Readdirnames(0)
	if err != nil {
		return "", err
	}
	sort.Slice(names, func(i, j int) bool {
		return i < j
	})

	return filepath.Join(logDir, names[len(names)-1]), nil
}
