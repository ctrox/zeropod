package activator

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf redirector.c -- -I/headers

const (
	BPFFSPath                      = "/sys/fs/bpf"
	probeBinaryNameVariable        = "probe_binary_name"
	probeBinaryNameMaxLength       = 16
	SocketTrackerMap               = "socket_tracker"
	PodKubeletAddrsMapv4           = "kubelet_addrs_v4"
	PodKubeletAddrsMapv6           = "kubelet_addrs_v6"
	trackerIgnoreLocalhostVariable = "tracker_ignore_localhost"
	taskCommOffsetVariable         = "task_comm_offset"
	tcxIngressPinName              = "tcx_ingress"
	tcxEgressPinName               = "tcx_egress"
	ManagedByShimSuffix            = "_managed_by_shim"
)

type BPF struct {
	pid     int
	objs    *bpfObjects
	log     *slog.Logger
	qdiscs  []*netlink.GenericQdisc
	filters []*netlink.BpfFilter
	links   []link.Link
	noPin   bool
}

type BPFConfig struct {
	mapSizes               map[string]uint32
	probeBinaryName        string
	trackerIgnoreLocalhost bool
	disablePinning         bool
	managedByShim          bool
}

type BPFOpts func(cfg *BPFConfig)

func OverrideMapSize(mapSizes map[string]uint32) BPFOpts {
	return func(cfg *BPFConfig) {
		maps.Copy(cfg.mapSizes, mapSizes)
	}
}

func ProbeBinaryName(name string) BPFOpts {
	return func(cfg *BPFConfig) {
		cfg.probeBinaryName = name
	}
}

func TrackerIgnoreLocalhost(ignore bool) BPFOpts {
	return func(cfg *BPFConfig) {
		cfg.trackerIgnoreLocalhost = ignore
	}
}

func DisablePinning() BPFOpts {
	return func(cfg *BPFConfig) {
		cfg.disablePinning = true
	}
}

func ShimManaged() BPFOpts {
	return func(cfg *BPFConfig) {
		cfg.managedByShim = true
	}
}

func InitBPF(pid int, log *slog.Logger, opts ...BPFOpts) (*BPF, error) {
	cfg := &BPFConfig{
		mapSizes: map[string]uint32{
			SocketTrackerMap: 128,
		},
	}
	for _, opt := range opts {
		opt(cfg)
	}
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}

	// as a single shim process can host multiple pods, we store the map in a
	// directory per sandbox pid.
	pinPath := PinPath(pid)
	if cfg.managedByShim {
		if err := os.MkdirAll(pinPath+ManagedByShimSuffix, os.ModePerm); err != nil {
			return nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
		}
	}
	if err := os.MkdirAll(pinPath, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
	}

	spec, err := loadBpf()
	if err != nil {
		return nil, fmt.Errorf("loading bpf objects: %w", err)
	}

	if len([]byte(cfg.probeBinaryName)) > probeBinaryNameMaxLength {
		return nil, fmt.Errorf(
			"probe binary name %s is too long (%d bytes), max is %d bytes",
			cfg.probeBinaryName, len([]byte(cfg.probeBinaryName)), probeBinaryNameMaxLength,
		)
	}
	binName := [probeBinaryNameMaxLength]byte{}
	copy(binName[:], cfg.probeBinaryName[:])
	if err := setVar(spec, probeBinaryNameVariable, binName); err != nil {
		return nil, fmt.Errorf("setting probe binary variable: %w", err)
	}
	if err := setVar(spec, trackerIgnoreLocalhostVariable, cfg.trackerIgnoreLocalhost); err != nil {
		return nil, fmt.Errorf("setting trackerIgnoreLocalhost variable: %w", err)
	}
	// in case you wonder why we do this instead of just accessing comm directly
	// in the task struct in eBPF: this would make the btf loading way more
	// expensive, at least the way cilium/ebpf behaves at the moment. It is
	// about 3x slower and uses 3x the amount of memory. This is rather ugly but
	// I think the performance gains are worth it.
	commOffset, err := getTaskCommOffset()
	if err != nil {
		return nil, err
	}
	if err := setVar(spec, taskCommOffsetVariable, commOffset); err != nil {
		return nil, fmt.Errorf("setting probe binary variable: %w", err)
	}

	for mapName, size := range cfg.mapSizes {
		spec.Maps[mapName].MaxEntries = size
	}
	objs := bpfObjects{}
	if err := spec.LoadAndAssign(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: pinPath,
		},
	}); err != nil {
		return nil, fmt.Errorf("loading objects: %w", err)
	}

	return &BPF{pid: pid, log: log, objs: &objs, noPin: cfg.disablePinning}, nil
}

// ManagedByShim returns true if loading/pinning is managed by the shim itself.
func ManagedByShim(pid int) bool {
	if _, err := os.Stat(PinPath(pid) + ManagedByShimSuffix); err == nil {
		return true
	}
	return false
}

// TCXPinned returns true if all TCX programs for the pid are pinned.
func TCXPinned(pid int, ifaces ...string) bool {
	for _, iface := range ifaces {
		for _, attach := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
			_, err := os.Stat(tcxLinkPath(pid, iface, attach))
			if err != nil {
				return false
			}
		}
	}
	return true
}

func setVar(spec *ebpf.CollectionSpec, name string, value any) error {
	if _, ok := spec.Variables[name]; !ok {
		return fmt.Errorf("could not find var %s in spec", name)
	}
	if err := spec.Variables[name].Set(value); err != nil {
		return fmt.Errorf("setting spec variable: %w", err)
	}
	return nil
}

func getTaskCommOffset() (uint32, error) {
	kernelSpec, err := btf.LoadKernelSpec()
	if err != nil {
		return 0, fmt.Errorf("failed to load kernel BTF: %w", err)
	}
	var taskStruct *btf.Struct
	if err := kernelSpec.TypeByName("task_struct", &taskStruct); err != nil {
		return 0, fmt.Errorf("task_struct not found in kernel BTF: %w", err)
	}
	for _, member := range taskStruct.Members {
		if member.Name == "comm" {
			return member.Offset.Bytes(), nil
		}
	}
	return 0, fmt.Errorf("comm field not found in task_struct")
}

func (bpf *BPF) Cleanup() error {
	errs := []error{}
	for _, link := range bpf.links {
		if err := link.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing link: %w", err))
		}
	}
	if bpf.objs != nil {
		if err := bpf.objs.Close(); err != nil {
			errs = append(errs, fmt.Errorf("unable to close bpf objects: %w", err))
		}
	}
	for _, qdisc := range bpf.qdiscs {
		if err := netlink.QdiscDel(qdisc); !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("unable to delete qdisc: %w", err))
		}
	}
	for _, filter := range bpf.filters {
		if err := netlink.FilterDel(filter); !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("unable to delete filter: %w", err))
		}
	}

	bpf.log.Info("deleting", "path", PinPath(bpf.pid))
	errs = append(errs, cleanPinPath(bpf.pid))
	return errors.Join(errs...)
}

func cleanPinPath(pid int) error {
	return errors.Join(
		os.RemoveAll(PinPath(pid)),
		os.RemoveAll(PinPath(pid)+ManagedByShimSuffix),
	)
}

func (bpf *BPF) AttachInNetNS(pid int, ifaces ...string) error {
	netNS, err := ns.GetNS(netNSPath(pid))
	if err != nil {
		return err
	}
	if err := netNS.Do(func(nn ns.NetNS) error {
		if err := bpf.AttachRedirector(ifaces...); err != nil {
			return err
		}
		return err
	}); err != nil {
		return errors.Join(err, bpf.Cleanup())
	}
	return nil
}

func (bpf *BPF) AttachRedirector(ifaces ...string) error {
	for _, ifaceName := range ifaces {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return fmt.Errorf("could not get interface ID: %w", err)
		}
		// try TCX first and if that is unsupported by the kernel fallback to
		// the old qdisc.
		tcxErr := bpf.attachTCX(iface)
		if tcxErr == nil {
			bpf.log.Info("attached redirector using TCX", "iface", ifaceName)
			continue
		}
		if err := bpf.attachQdisc(iface); err == nil {
			bpf.log.Warn("attached redirector using Qdisc as TCX failed", "iface", ifaceName, "error", tcxErr)
			return err
		}
	}
	return nil
}

func (bpf *BPF) attachTCX(iface *net.Interface) error {
	ingress, err := bpf.loadOrAttachTCXLink(iface, bpf.objs.TcRedirectIngress, ebpf.AttachTCXIngress)
	if err != nil {
		return fmt.Errorf("loading TCX: %w", err)
	}
	bpf.links = append(bpf.links, ingress)
	egress, err := bpf.loadOrAttachTCXLink(iface, bpf.objs.TcRedirectEgress, ebpf.AttachTCXEgress)
	if err != nil {
		return fmt.Errorf("could not attach egress TCX: %w", err)
	}
	bpf.links = append(bpf.links, egress)
	return nil
}

func (bpf *BPF) loadOrAttachTCXLink(iface *net.Interface, program *ebpf.Program, attach ebpf.AttachType) (link.Link, error) {
	pinPath := tcxLinkPath(bpf.pid, iface.Name, attach)
	l, err := link.LoadPinnedLink(pinPath, nil)
	if err == nil {
		return l, nil
	}
	l, err = link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   program,
		Attach:    attach,
	})
	if err != nil {
		return nil, fmt.Errorf("could not attach TCX %s: %w", pinPath, err)
	}
	if bpf.noPin {
		return l, nil
	}
	return l, l.Pin(pinPath)
}

func tcxLinkPath(pid int, ifaceName string, attach ebpf.AttachType) string {
	name := tcxIngressPinName
	if attach == ebpf.AttachTCXEgress {
		name = tcxEgressPinName
	}
	return filepath.Join(PinPath(pid), fmt.Sprintf("%s_%s", name, ifaceName))
}

func (bpf *BPF) attachQdisc(iface *net.Interface) error {
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: iface.Index,
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
			LinkIndex: iface.Index,
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
	return nil
}

func PinPath(pid int) string {
	return filepath.Join(MapsPath(), strconv.Itoa(pid))
}

func MapsPath() string {
	return filepath.Join(BPFFSPath, "zeropod_maps")
}

// MountBPFFS executes a bpf mount on the supplied path. It has been adapted by:
// https://github.com/cilium/cilium/blob/cf3889af46a4058d5e89495d502fc19c10713110/pkg/bpf/bpffs_linux.go#L124
func MountBPFFS(path string) error {
	mapRootStat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path, 0755); err != nil {
				return fmt.Errorf("unable to create bpf mount directory: %w", err)
			}
		} else {
			return fmt.Errorf("failed to stat the mount path %s: %w", path, err)
		}
	} else if !mapRootStat.IsDir() {
		return fmt.Errorf("%s is a file which is not a directory", path)
	}

	if err := unix.Mount(path, path, "bpf", 0, ""); err != nil {
		return fmt.Errorf("failed to mount %s: %w", path, err)
	}
	return nil
}
