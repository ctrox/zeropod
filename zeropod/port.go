package zeropod

import (
	"path/filepath"
	"strconv"

	"github.com/prometheus/procfs"
)

const (
	stateListen = 10
	procPath    = "/proc"
)

// ListeningPorts finds all ports of the pid that are in listen state of the
// supplied process. It finds both, ipv4 and ipv6 sockets.
func ListeningPorts(pid int) ([]uint16, error) {
	fs, err := procfs.NewFS(filepath.Join(procPath, strconv.Itoa(pid)))
	if err != nil {
		return nil, err
	}

	tcp, err := fs.NetTCP()
	if err != nil {
		return nil, err
	}

	tcp6, err := fs.NetTCP6()
	if err != nil {
		return nil, err
	}

	// use a map to eliminate duplicates
	portMap := map[uint16]struct{}{}

	for _, line := range append(tcp, tcp6...) {
		if line.St == stateListen {
			portMap[uint16(line.LocalPort)] = struct{}{}
		}
	}

	ports := []uint16{}
	for k := range portMap {
		ports = append(ports, k)
	}

	return ports, err
}
