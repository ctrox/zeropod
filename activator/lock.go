package activator

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/containerd/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

type NetworkLockerBackend string

type NetworkLocker interface {
	Backend() NetworkLockerBackend
	Lock(ports []uint16) error
	Unlock(ports []uint16) error
}

const (
	NetworkLockerNFTables NetworkLockerBackend = "nftables"
	NetworkLockerIPTables NetworkLockerBackend = "iptables"
)

// NewNetworkLocker returns a new network locker with the implementation
// depending on OS support. If supported it will return an nftables based
// locker with a fallback to iptables.
func NewNetworkLocker(netNS ns.NetNS) NetworkLocker {
	if nftablesSupported() {
		return &nftablesLocker{netNS: netNS}
	}

	return &iptablesLocker{netNS: netNS}
}

type nftablesLocker struct {
	netNS ns.NetNS
}

func (n *nftablesLocker) Backend() NetworkLockerBackend {
	return NetworkLockerNFTables
}

// LockNetwork "locks" the network by adding a nftables rule that drops all
// incoming traffic in the specified network namespace.
func (n *nftablesLocker) Lock(ports []uint16) error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(n.netNS.Fd())))
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	table := &nftables.Table{Name: tableName(ports), Family: nftables.TableFamilyINet}

	nft.AddTable(table)
	chain := &nftables.Chain{
		Name:     "input",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
	}

	nft.AddChain(chain)

	// the nftables package is a *bit* low-level. This results in "tcp dport <port> drop".
	for _, port := range ports {
		rule := &nftables.Rule{
			Table: table,
			Chain: chain,
			Exprs: []expr.Any{
				// TCP
				&expr.Meta{
					Key:            expr.MetaKeyL4PROTO,
					SourceRegister: false,
					Register:       1,
				},
				&expr.Cmp{
					Op:       expr.CmpOpEq,
					Register: 1,
					Data:     []byte{unix.IPPROTO_TCP},
				},
				// dport <port>
				&expr.Payload{
					OperationType:  expr.PayloadLoad,
					DestRegister:   1,
					SourceRegister: 0,
					Base:           expr.PayloadBaseTransportHeader,
					Offset:         2,
					Len:            2,
					CsumType:       0,
					CsumOffset:     0,
					CsumFlags:      0,
				},
				&expr.Cmp{
					Op:       0,
					Register: 1,
					Data:     binaryutil.BigEndian.PutUint16(port),
				},
				// drop
				&expr.Verdict{
					Kind: expr.VerdictDrop,
				},
			},
		}
		nft.AddRule(rule)
	}

	return nft.Flush()
}

// Unlock removes the netfilter table created by Lock.
func (n *nftablesLocker) Unlock(ports []uint16) error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(n.netNS.Fd())))
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	nft.DelTable(&nftables.Table{
		Name:   tableName(ports),
		Family: nftables.TableFamilyINet},
	)

	return nft.Flush()
}

type iptablesLocker struct {
	netNS ns.NetNS
}

func (n *iptablesLocker) Backend() NetworkLockerBackend {
	return NetworkLockerIPTables
}

// Lock "locks" the network by adding iptables rules that drops all
// incoming traffic in the specified network namespace.
func (n *iptablesLocker) Lock(ports []uint16) error {
	table := tableName(ports)
	targets := ""
	for _, port := range ports {
		targets += fmt.Sprintf(`
-A INPUT -j %[1]s -p tcp --dport %[2]d
`, table, port)
	}

	conf := fmt.Sprintf(`*filter
:%[1]s - [0:0]
%[2]s
-A %[1]s -j DROP
COMMIT
`, table, targets)

	return ipTablesRestore(conf, n.netNS)
}

func portsToString(ports []uint16) []string {
	portList := make([]string, 0, len(ports))
	for _, port := range ports {
		portList = append(portList, strconv.Itoa(int(port)))
	}

	return portList
}

func tableName(ports []uint16) string {
	return fmt.Sprintf("ZEROPOD_%s", strings.Join(portsToString(ports), "_"))
}

// Unlock removes all iptables rules in the network namespace.
func (n *iptablesLocker) Unlock(ports []uint16) error {
	const conf = "*filter\n" +
		":INPUT ACCEPT [0:0]\n" +
		":FORWARD ACCEPT [0:0]\n" +
		":OUTPUT ACCEPT [0:0]\n" +
		"COMMIT\n"

	return ipTablesRestore(conf, n.netNS)
}

func ipTablesRestore(conf string, netNS ns.NetNS) error {
	cmd := exec.Command("iptables-restore")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	errors := make(chan error)
	go func() {
		defer stdin.Close()
		_, err := io.WriteString(stdin, conf)
		if err != nil {
			errors <- err
		}
		close(errors)
	}()

	const errNoChild = "wait: no child processes"
	if err := netNS.Do(func(nn ns.NetNS) error {
		out, err := cmd.CombinedOutput()
		if err != nil {
			if err.Error() == errNoChild {
				// not 100% confident but this is probably fine and means the
				// process already exited when Wait() was called. But as I
				// can't reproduce it locally it's a bit hard to say. For now
				// we log it and return nil.
				log.L.Warnf("iptables-restore might have failed with: %s", err)
				return nil
			}
			return fmt.Errorf("error running iptables-restore: %s: %w", out, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return <-errors
}

// nftablesSupported returns true if nftables is supported on the system.
func nftablesSupported() bool {
	nft, err := nftables.New()
	if err == nil {
		// not sure if there's a quicker way to test this but this seems to
		// work well enough.
		if _, err := nft.ListChains(); err == nil {
			return true
		}
	}

	return false
}
