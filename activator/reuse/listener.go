package reuse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

type wakeListener struct {
	ln    *net.TCPListener
	lnFd  *os.File
	reuse *reuseportObjects
}

type appListener struct {
	fd int
}

type listenerKey struct {
	port    uint16
	network network
}

type listener struct {
	port   uint16
	family uint8
	inode  uint32
	origFd int
	fd     *os.File
}

var ErrNoListeningSockets = errors.New("no listening sockets found")

func (wl *wakeListener) closeListener() {
	if wl.ln != nil {
		_ = wl.ln.Close()
	}
	if wl.lnFd != nil {
		_ = wl.lnFd.Close()
	}
}

func (wl *wakeListener) close() {
	wl.closeListener()
	if wl.reuse != nil {
		_ = wl.reuse.Close()
	}
}

func (act *Activator) listenWake(port uint16, network network, wl *wakeListener) error {
	if err := act.ns.Do(func(nn ns.NetNS) error {
		ln, err := listenReuseport(port, network)
		if err != nil {
			return fmt.Errorf("wake listener: %w", err)
		}
		wl.ln = ln
		return nil
	}); err != nil {
		return err
	}
	f, err := wl.ln.File()
	if err != nil {
		wl.ln.Close()
		return err
	}
	wl.lnFd = f
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	act.wakeInodes = append(act.wakeInodes, stat.Ino)
	if err := unix.SetsockoptInt(int(wl.lnFd.Fd()), unix.SOL_SOCKET,
		unix.SO_ATTACH_REUSEPORT_EBPF, wl.reuse.SelectOrMigrate.FD()); err != nil {
		return fmt.Errorf("attach reuseport prog: %w", err)
	}
	key := uint32(wakeKey)
	if err := wl.reuse.Listeners.Update(&key, uint64(wl.lnFd.Fd()), ebpf.UpdateAny); err != nil {
		wl.lnFd.Close()
		wl.ln.Close()
		act.log.WithError(err).Error("inserting wake listener")
		return fmt.Errorf("inserting wake listener: %w", err)
	}
	go act.watchWake(wl.lnFd, network)
	return nil
}

// watchWake polls the wake listener without ever accepting and calls wake as
// soon as the poll returns something.
func (act *Activator) watchWake(f *os.File, network network) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		act.log.WithError(err).Error("epoll create")
		return
	}
	defer func() {
		_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, int(f.Fd()), nil)
		_ = unix.Close(epfd)
	}()
	event := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(f.Fd()),
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, int(f.Fd()), &event); err != nil {
		act.log.WithError(err).Error("failed to register socket with epoll")
		return
	}
	events := make([]unix.EpollEvent, 10)
	for {
		n, err := unix.EpollWait(epfd, events, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			act.log.WithError(err).Error("epoll wait failed")
		}

		for i := range n {
			if int(events[i].Fd) != int(f.Fd()) {
				continue
			}
			act.log.Info("socket activity detected, waking up")
			if err := act.wake(network); err != nil {
				act.log.WithError(err).Error("wake")
			}

			return
		}
	}
}

func (act *Activator) registerListeners(pid int) error {
	act.mu.Lock()
	defer act.mu.Unlock()

	before := time.Now()
	listeners, err := act.listenerFds(pid, len(act.ports))
	if err != nil {
		return err
	}
	if len(listeners) < len(act.ports) {
		return fmt.Errorf("%w: expected at least %d listeners, found %d", ErrNoListeningSockets, len(act.ports), len(listeners))
	}
	act.log.Infof("getting listeners in %s", time.Since(before))

	for _, l := range listeners {
		if l.fd == nil {
			continue
		}
		defer l.fd.Close()
		act.log.Infof("registering ln %d port %d family %d ino %d", l.fd.Fd(), l.port, l.family, l.inode)
		objs := &reuseportObjects{}
		if err := loadReuseportObjects(objs, &ebpf.CollectionOptions{}); err != nil {
			return fmt.Errorf("loading reuseport objects: %w", err)
		}
		if err := act.registerLn(l.fd, objs.Listeners, objs.SelectOrMigrate); err != nil {
			return fmt.Errorf("registering listener: %w", err)
		}
		net := networkTCP4
		if l.family == unix.AF_INET6 {
			net = networkTCP6ONLY
		}
		key := listenerKey{port: l.port, network: net}
		wl, ok := act.wakeListeners[key]
		if !ok {
			act.wakeListeners[key] = &wakeListener{reuse: objs}
		} else {
			wl.reuse = objs
		}
		act.log.Infof("caching port %d fd %d", l.port, l.origFd)
		act.appListeners[key] = &appListener{fd: l.origFd}
	}
	if len(listeners) == 0 {
		return ErrNoListeningSockets
	}
	return nil
}

func (act *Activator) listenerFds(pid int, minListeners int) ([]listener, error) {
	l, err := act.listenerFdsFromCache(pid)
	if err == nil && len(l) >= minListeners {
		return l, nil
	}
	listeners, err := act.getListeningInodes(pid)
	if err != nil {
		return nil, err
	}

	pids, err := containerPids(pid)
	if err != nil {
		return nil, err
	}

	listenersWithFd := []listener{}
	inos := map[uint32]struct{}{}
	for _, cpid := range pids {
		for _, listener := range listeners {
			if _, ok := inos[listener.inode]; ok {
				continue
			}
			if slices.Contains(act.wakeInodes, uint64(listener.inode)) {
				continue
			}
			target, err := socketFdNum(cpid, []uint32{listener.inode})
			if err != nil {
				continue
			}
			pidfd, err := unix.PidfdOpen(cpid, 0)
			if err != nil {
				continue
			}
			defer unix.Close(pidfd)

			fd, err := unix.PidfdGetfd(pidfd, target, 0)
			if err != nil {
				continue
			}
			listener.fd = os.NewFile(uintptr(fd), "")
			listener.origFd = target
			listenersWithFd = append(listenersWithFd, listener)
			inos[listener.inode] = struct{}{}
		}
	}
	return listenersWithFd, nil
}

// registerLn attaches select_or_migrate to the listerners reuseport group and
// puts it into slot 0. The caller closes its dup of the fd afterwards and the
// sockarray holds the socket via the listeners own fd.
func (act *Activator) registerLn(lnFd *os.File, bpfMap *ebpf.Map, prog *ebpf.Program) error {
	if err := unix.SetsockoptInt(int(lnFd.Fd()), unix.SOL_SOCKET,
		unix.SO_ATTACH_REUSEPORT_EBPF, prog.FD()); err != nil {
		return fmt.Errorf("attach reuseport prog: %w", err)
	}
	key := uint32(appKey)
	if err := bpfMap.Update(&key, uint64(lnFd.Fd()), ebpf.UpdateAny); err != nil {
		return fmt.Errorf("sockarray app: %w", err)
	}
	return nil
}

// listenReuseport opens a TCP listener with SO_REUSEPORT
func listenReuseport(port uint16, network network) (*net.TCPListener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if serr != nil {
					return
				}
				if network == networkTCP6ONLY {
					serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
				}
			}); err != nil {
				return err
			}
			return serr
		},
	}
	lc.SetMultipathTCP(false)
	n := string(network)
	if network == networkTCP6ONLY {
		n = string(networkTCP6)
	}
	ln, err := lc.Listen(context.Background(), n, fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	return ln.(*net.TCPListener), nil
}

func (act *Activator) listenerFdsFromCache(pid int) ([]listener, error) {
	cache := act.appListeners
	if len(cache) == 0 {
		return nil, nil
	}

	listeners := []listener{}
	pids, err := containerPids(pid)
	if err != nil {
		return nil, err
	}
	for _, cpid := range pids {
		// bail out early as we found all listeners
		if len(listeners) == len(cache) {
			break
		}

		pidfd, err := unix.PidfdOpen(cpid, 0)
		if err != nil {
			continue
		}
		defer unix.Close(pidfd)

		for k, v := range cache {
			fd, err := unix.PidfdGetfd(pidfd, v.fd, 0)
			if err != nil {
				continue
			}
			var stat unix.Stat_t
			if err := unix.Fstat(int(fd), &stat); err != nil {
				return nil, err
			}

			sockaddr, err := unix.Getsockname(fd)
			if err != nil {
				continue
			}

			var port int
			switch sa := sockaddr.(type) {
			case *unix.SockaddrInet4:
				port = sa.Port
			case *unix.SockaddrInet6:
				port = sa.Port
			}
			if k.port != uint16(port) {
				continue
			}

			family := uint8(unix.AF_INET)
			if k.network == networkTCP6 || k.network == networkTCP6ONLY {
				family = unix.AF_INET6
			}
			listeners = append(listeners, listener{
				port:   k.port,
				family: family,
				fd:     os.NewFile(uintptr(fd), ""),
				origFd: v.fd,
				inode:  uint32(stat.Ino),
			})
		}
	}
	return listeners, nil
}

func containerPids(pid int) ([]int, error) {
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

func (act *Activator) getListeningInodes(pid int) ([]listener, error) {
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

	listeners := []listener{}
	const tcpListen = 10
	for _, sock := range netTCP4 {
		if sock.St == tcpListen {
			if slices.Contains(act.wakeInodes, uint64(sock.Inode)) {
				continue
			}
			listeners = append(listeners, listener{
				port:   uint16(sock.LocalPort),
				family: unix.AF_INET,
				inode:  uint32(sock.Inode),
			})
		}
	}
	for _, sock := range netTCP6 {
		if sock.St == tcpListen {
			if slices.Contains(act.wakeInodes, uint64(sock.Inode)) {
				continue
			}
			listeners = append(listeners, listener{
				port:   uint16(sock.LocalPort),
				family: unix.AF_INET6,
				inode:  uint32(sock.Inode),
			})
		}
	}

	if len(listeners) == 0 {
		return nil, ErrNoListeningSockets
	}
	return listeners, nil
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
