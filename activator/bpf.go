package activator

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/ctrox/zeropod/socket"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf redirector.c -- -I/headers

func initBPF(ifaces ...string) (*bpfObjects, func(), error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, nil, err
	}

	// as a single shim process can host multiple containers, we store the map
	// in a directory per shim process.
	path := pinPath()
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		return nil, nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
	}

	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: path,
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("loading objects: %w", err)
	}

	qdiscs := []*netlink.GenericQdisc{}
	filters := []*netlink.BpfFilter{}

	for _, iface := range ifaces {
		devID, err := net.InterfaceByName(iface)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get interface ID: %w", err)
		}

		qdisc := &netlink.GenericQdisc{
			QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: devID.Index,
				Handle:    netlink.MakeHandle(0xffff, 0),
				Parent:    netlink.HANDLE_CLSACT,
			},
			QdiscType: "clsact",
		}

		if err := netlink.QdiscReplace(qdisc); err != nil {
			return nil, nil, fmt.Errorf("could not replace qdisc: %w", err)
		}
		qdiscs = append(qdiscs, qdisc)

		ingress := netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: devID.Index,
				Parent:    netlink.HANDLE_MIN_INGRESS,
				Handle:    1,
				Protocol:  unix.ETH_P_ALL,
			},
			Fd:           objs.TcRedirectIngress.FD(),
			Name:         objs.TcRedirectIngress.String(),
			DirectAction: true,
		}
		egress := ingress
		egress.Parent = netlink.HANDLE_MIN_EGRESS
		egress.Fd = objs.TcRedirectEgress.FD()
		egress.Name = objs.TcRedirectEgress.String()

		if err := netlink.FilterReplace(&ingress); err != nil {
			return nil, nil, fmt.Errorf("failed to replace tc filter: %w", err)
		}
		filters = append(filters, &ingress)

		if err := netlink.FilterReplace(&egress); err != nil {
			return nil, nil, fmt.Errorf("failed to replace tc filter: %w", err)
		}
		filters = append(filters, &egress)
	}

	return &objs, func() {
		for _, qdisc := range qdiscs {
			netlink.QdiscDel(qdisc)
		}
		for _, filter := range filters {
			netlink.FilterDel(filter)
		}
		objs.Close()
		os.RemoveAll(pinPath())
	}, nil
}

func pinPath() string {
	return filepath.Join(socket.BPFFSPath, "zeropod_maps", strconv.Itoa(os.Getpid()))
}

// RedirectPort redirects the port from to on ingress and to from on egress.
func (a *Server) RedirectPort(from, to uint16) error {
	if err := a.bpfObjs.IngressRedirects.Put(&from, &to); err != nil {
		return fmt.Errorf("unable to put ports %d -> %d into bpf map: %w", from, to, err)
	}
	if err := a.bpfObjs.EgressRedirects.Put(&to, &from); err != nil {
		return fmt.Errorf("unable to put ports %d -> %d into bpf map: %w", to, from, err)
	}
	return nil
}

func (a *Server) registerConnection(port uint16) error {
	if err := a.bpfObjs.ActiveConnections.Put(&port, uint8(1)); err != nil {
		return fmt.Errorf("unable to put port %d into bpf map: %w", port, err)
	}
	return nil
}

func (a *Server) removeConnection(port uint16) error {
	if err := a.bpfObjs.ActiveConnections.Delete(&port); err != nil {
		return fmt.Errorf("unable to delete port %d in bpf map: %w", port, err)
	}
	return nil
}

func (a *Server) disableRedirect(port uint16) error {
	if err := a.bpfObjs.DisableRedirect.Put(&port, uint8(1)); err != nil {
		return fmt.Errorf("unable to put %d into bpf map: %w", port, err)
	}
	return nil
}

func (a *Server) enableRedirect(port uint16) error {
	if err := a.bpfObjs.DisableRedirect.Delete(&port); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return err
		}
	}
	return nil
}
