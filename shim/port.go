package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
)

const (
	stateListen  = 10
	procPath     = "/proc"
	childrenFile = "children"
	taskDir      = "task"
)

// listeningPorts finds all ports of the pid that are in listen state of the
// supplied process and all child processes. It finds both, ipv4 and ipv6
// sockets.
func listeningPortsDeep(pid int) ([]uint16, error) {
	children, err := findChildren(pid)
	if err != nil {
		return nil, fmt.Errorf("finding child pids: %w", err)
	}

	// use a map to eliminate duplicates
	portMap := map[uint16]struct{}{}
	for _, pid := range append([]int{pid}, children...) {
		p, err := listeningPorts(pid)
		if err != nil {
			return nil, err
		}
		for _, port := range p {
			portMap[port] = struct{}{}
		}
	}
	ports := []uint16{}
	for k := range portMap {
		ports = append(ports, k)
	}

	return ports, nil
}

// listeningPorts finds all ports of the pid that are in listen state of the
// supplied process. It finds both, ipv4 and ipv6 sockets.
func listeningPorts(pid int) ([]uint16, error) {
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

	inos, err := inodes(pid)
	if err != nil {
		return nil, err
	}

	// use a map to eliminate duplicates
	portMap := map[uint16]struct{}{}
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

// findChildren uses the procfs to find all child tasks of the supplied
// process and returns their pids if any are found.
func findChildren(pid int) ([]int, error) {
	taskPath := filepath.Join(procPath, strconv.Itoa(pid), taskDir)
	tasks, err := os.ReadDir(taskPath)
	if err != nil {
		return nil, fmt.Errorf("listing tasks dir: %w", err)
	}

	pids := []int{}
	for _, task := range tasks {
		f, err := os.ReadFile(filepath.Join(taskPath, task.Name(), childrenFile))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading children file: %w", err)
		}

		spl := strings.Split(string(f), " ")
		if len(spl) < 2 {
			continue
		}

		for _, strPID := range spl {
			pid, err := strconv.Atoi(strPID)
			if err != nil {
				continue
			}
			pids = append(pids, pid)
		}
	}

	return pids, nil
}
