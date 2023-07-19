package zeropod

import (
	"fmt"
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
	inos, err := inodes(pid)
	if err != nil {
		return nil, err
	}

	for _, line := range append(tcp, tcp6...) {
		if _, ok := inos[line.Inode]; ok && line.St == stateListen {
			portMap[uint16(line.LocalPort)] = struct{}{}
		}
	}

	ports := []uint16{}
	for k := range portMap {
		ports = append(ports, k)
	}

	return ports, err
}

func inodes(pid int) (map[uint64]struct{}, error) {
	fs, err := procfs.NewFS(procPath)
	if err != nil {
		return nil, err
	}

	proc, err := fs.Proc(pid)
	if err != nil {
		return nil, err
	}

	fdInfos, err := proc.FileDescriptorsInfo()
	if err != nil {
		return nil, err
	}

	inodes := map[uint64]struct{}{}
	for _, fdInfo := range fdInfos {
		inode, err := strconv.ParseUint(fdInfo.Ino, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unable to parse inode to uint: %w", err)
		}
		inodes[inode] = struct{}{}
	}

	return inodes, nil
}
