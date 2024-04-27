package activator

import (
	"fmt"
	"log/slog"
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

type BPF struct {
	pid     int
	objs    *bpfObjects
	qdiscs  []*netlink.GenericQdisc
	filters []*netlink.BpfFilter
}

func InitBPF(pid int) (*BPF, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}

	// as a single shim process can host multiple containers, we store the map
	// in a directory per shim process.
	path := PinPath(pid)
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
	}

	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: path,
		},
	}); err != nil {
		return nil, fmt.Errorf("loading objects: %w", err)
	}

	return &BPF{pid: pid, objs: &objs}, nil
}

func (bpf *BPF) Cleanup() error {
	if err := bpf.objs.Close(); err != nil {
		return fmt.Errorf("unable to close bpf objects: %w", err)
	}

	for _, qdisc := range bpf.qdiscs {
		if err := netlink.QdiscDel(qdisc); !os.IsNotExist(err) {
			return fmt.Errorf("unable to delete qdisc: %w", err)
		}
	}
	for _, filter := range bpf.filters {
		if err := netlink.FilterDel(filter); !os.IsNotExist(err) {
			return fmt.Errorf("unable to delete filter: %w", err)
		}
	}

	slog.Info("deleting", "path", PinPath(bpf.pid))
	return os.RemoveAll(PinPath(bpf.pid))
}

func (bpf *BPF) AttachRedirector(ifaces ...string) error {
	for _, iface := range ifaces {
		devID, err := net.InterfaceByName(iface)
		if err != nil {
			return fmt.Errorf("could not get interface ID: %w", err)
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
			return fmt.Errorf("could not replace qdisc: %w", err)
		}
		bpf.qdiscs = append(bpf.qdiscs, qdisc)

		ingress := netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: devID.Index,
				Parent:    netlink.HANDLE_MIN_INGRESS,
				Handle:    1,
				Protocol:  unix.ETH_P_ALL,
			},
			Fd:           bpf.objs.TcRedirectIngress.FD(),
			Name:         bpf.objs.TcRedirectIngress.String(),
			DirectAction: true,
		}
		egress := ingress
		egress.Parent = netlink.HANDLE_MIN_EGRESS
		egress.Fd = bpf.objs.TcRedirectEgress.FD()
		egress.Name = bpf.objs.TcRedirectEgress.String()

		if err := netlink.FilterReplace(&ingress); err != nil {
			return fmt.Errorf("failed to replace tc filter: %w", err)
		}
		bpf.filters = append(bpf.filters, &ingress)

		if err := netlink.FilterReplace(&egress); err != nil {
			return fmt.Errorf("failed to replace tc filter: %w", err)
		}
		bpf.filters = append(bpf.filters, &egress)
	}

	return nil
}

func PinPath(pid int) string {
	return filepath.Join(MapsPath(), strconv.Itoa(pid))
}

func MapsPath() string {
	return filepath.Join(socket.BPFFSPath, "zeropod_maps")
}
