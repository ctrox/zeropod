package activator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

type Network string

const (
	NetworkTCPAny   Network = "tcp"
	NetworkTCP4     Network = "tcp4"
	NetworkTCP6ONLY Network = "tcp6"
)

type Listener struct {
	Port    uint16   `json:"port"`
	Network Network  `json:"network"`
	UID     uint64   `json:"uid"`
	Inode   uint32   `json:"-"`
	OrigFd  int      `json:"-"`
	FD      *os.File `json:"-"`
	ownsFD  bool
}

type Listeners []Listener

func (lns Listeners) Ports() []uint16 {
	ports := map[uint16]struct{}{}
	for _, ln := range lns {
		ports[ln.Port] = struct{}{}
	}
	return slices.Collect(maps.Keys(ports))
}

func (ln Listener) OwnsFD() bool {
	return ln.ownsFD
}

var ErrNoListeningSockets = errors.New("no listening sockets found")

// GetListenersOfPID gets all [Listeners] in the pid namespace.
func GetListenersOfPID(ctx context.Context, pid int, ignoredInodes ...uint64) (Listeners, error) {
	return getListenersOfPID(ctx, pid, true, ignoredInodes...)
}

// GetListenersOfPIDWithFD gets all [Listeners] in the pid namespace.
// It's the callers responsibility to close the returned listener FDs.
func GetListenersOfPIDWithFD(ctx context.Context, pid int, ignoredInodes ...uint64) (Listeners, error) {
	return getListenersOfPID(ctx, pid, false, ignoredInodes...)
}

func getListenersOfPID(ctx context.Context, pid int, closeFD bool, ignoredInodes ...uint64) (Listeners, error) {
	fs, err := procfs.NewFS("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}

	netTCP4, err := fs.NetTCP()
	if err != nil {
		return nil, err
	}
	netTCP6, err := fs.NetTCP6()
	if err != nil {
		return nil, err
	}

	listeners := Listeners{}
	const tcpListen = 10
	for _, sock := range netTCP4 {
		if sock.St == tcpListen {
			if slices.Contains(ignoredInodes, uint64(sock.Inode)) {
				continue
			}
			listeners = append(listeners, Listener{
				Port:    uint16(sock.LocalPort),
				Network: NetworkTCP4,
				Inode:   uint32(sock.Inode),
				UID:     sock.UID,
			})
		}
	}
	for _, sock := range netTCP6 {
		if sock.St == tcpListen {
			if slices.Contains(ignoredInodes, uint64(sock.Inode)) {
				continue
			}
			listeners = append(listeners, Listener{
				Port:    uint16(sock.LocalPort),
				Network: NetworkTCPAny,
				Inode:   uint32(sock.Inode),
				UID:     sock.UID,
			})
		}
	}

	pids, err := ContainerPids(pid)
	if err != nil {
		return nil, err
	}

	inos := map[uint32]struct{}{}
	for _, cpid := range pids {
		for i, listener := range listeners {
			if _, ok := inos[listener.Inode]; ok {
				continue
			}
			if slices.Contains(ignoredInodes, uint64(listener.Inode)) {
				continue
			}
			target, err := socketFdNum(cpid, []uint32{listener.Inode})
			if err != nil {
				log.G(ctx).WithError(err).Debug("getting socket fd")
				continue
			}
			pidfd, err := unix.PidfdOpen(cpid, 0)
			if err != nil {
				log.G(ctx).WithError(err).Debug("pidfdopen")
				continue
			}
			defer unix.Close(pidfd)

			fd, err := unix.PidfdGetfd(pidfd, target, 0)
			if err != nil {
				log.G(ctx).WithError(err).Debug("pidfdgetfd")
				continue
			}

			sockaddr, err := unix.Getsockname(fd)
			if err != nil {
				log.G(ctx).WithError(err).Debug("getsockname")
				_ = unix.Close(fd)
				continue
			}
			var port int
			switch sa := sockaddr.(type) {
			case *unix.SockaddrInet4:
				port = sa.Port
			case *unix.SockaddrInet6:
				port = sa.Port
			}
			if listener.Port != uint16(port) {
				_ = unix.Close(fd)
				continue
			}
			// the network we get from the initial fs.NetTCP is inaccurate and
			// does not distinguish between dual-stack and tcp6-only so we get
			// it from the socket directly.
			network, err := GetNetworkFromSock(fd)
			if err != nil {
				log.G(ctx).WithError(err).Error("getting network from sock")
				_ = unix.Close(fd)
				continue
			}

			if closeFD {
				_ = unix.Close(fd)
			} else {
				listeners[i].FD = os.NewFile(uintptr(fd), "")
				listeners[i].ownsFD = true
			}
			listeners[i].Network = network
			listeners[i].OrigFd = target
			inos[listener.Inode] = struct{}{}
		}
	}

	if len(listeners) == 0 {
		return nil, ErrNoListeningSockets
	}
	return listeners, nil
}

// ContainerPids returns a slice of all pids in the same pidns of pid (including
// pid).
func ContainerPids(pid int) ([]int, error) {
	rootProc, err := procfs.NewProc(pid)
	if err != nil {
		return nil, err
	}
	rootNs, err := rootProc.Namespaces()
	if err != nil {
		return nil, err
	}
	pidNSInode := rootNs["pid"].Inode

	pfs, err := procfs.NewDefaultFS()
	if err != nil {
		return nil, err
	}
	procs, err := pfs.AllProcs()
	if err != nil {
		return nil, err
	}

	containerProcs := []int{}
	for _, proc := range procs {
		target, err := os.Readlink(filepath.Join(procfs.DefaultMountPoint, strconv.Itoa(proc.PID), "ns", "pid"))
		if err != nil {
			continue
		}

		fields := strings.SplitN(target, ":", 2)
		if len(fields) != 2 {
			continue
		}

		inode, err := strconv.ParseUint(strings.Trim(fields[1], "[]"), 10, 32)
		if err != nil {
			continue
		}

		if uint32(inode) != pidNSInode {
			continue
		}
		containerProcs = append(containerProcs, proc.PID)
	}
	return containerProcs, nil
}

// GetNetworkFromSock queries the socket fd to get the [Network] of the
// listening socket.
func GetNetworkFromSock(fd int) (Network, error) {
	domain, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN)
	if err != nil {
		return Network(""), err
	}
	if domain == unix.AF_INET6 {
		if v, err := unix.GetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY); err == nil && v == 1 {
			return NetworkTCP6ONLY, nil
		}
		return NetworkTCPAny, nil
	}
	return NetworkTCP4, nil
}

// socketFdNum scans /proc/<pid>/fd for the fd number backing any of the
// socket inodes.
func socketFdNum(pid int, inodes []uint32) (int, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	want := make(map[string]bool, len(inodes))
	for _, ino := range inodes {
		want[fmt.Sprintf("socket:[%d]", ino)] = true
	}
	for _, e := range ents {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || !want[link] {
			continue
		}
		return strconv.Atoi(e.Name())
	}
	return 0, fmt.Errorf("no fd for socket inodes %v", inodes)
}
