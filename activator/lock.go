package activator

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/containerd/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

type UnlockOptions struct {
	CriuPid int
}

type NetworkLockerBackend string

type NetworkLocker interface {
	Backend() NetworkLockerBackend
	Lock() error
	Unlock(UnlockOptions) error
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
func (n *nftablesLocker) Lock() error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(n.netNS.Fd())))
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	table := &nftables.Table{Name: "zeropod", Family: nftables.TableFamilyINet}

	nft.AddTable(table)
	chain := &nftables.Chain{
		Name:     "input",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
	}

	nft.AddChain(chain)

	rule := &nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			&expr.Verdict{
				Kind: expr.VerdictDrop,
			},
		},
	}

	nft.AddRule(rule)

	return nft.Flush()
}

// Unlock removes the netfilter table created by LockNetwork. Additionally it
// also removes any CRIU-created tables if the pid of the restored process is
// given.
func (n *nftablesLocker) Unlock(opts UnlockOptions) error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(n.netNS.Fd())))
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	if opts.CriuPid != 0 {
		nft.DelTable(&nftables.Table{Name: "CRIU-" + strconv.Itoa(opts.CriuPid), Family: nftables.TableFamilyINet})
	}

	nft.DelTable(&nftables.Table{Name: "zeropod", Family: nftables.TableFamilyINet})

	return nft.Flush()
}

type iptablesLocker struct {
	netNS ns.NetNS
}

func (n *iptablesLocker) Backend() NetworkLockerBackend {
	return NetworkLockerNFTables
}

// LockNetwork "locks" the network by adding iptables rules that drops all
// incoming traffic in the specified network namespace. These are the same
// rules that CRIU also creates during checkpoint.
func (n *iptablesLocker) Lock() error {
	const SOCCR_MARK = "0xC114"
	const conf = "*filter\n" +
		":CRIU - [0:0]\n" +
		"-I INPUT -j CRIU\n" +
		"-I OUTPUT -j CRIU\n" +
		"-A CRIU -m mark --mark " + SOCCR_MARK + " -j ACCEPT\n" +
		"-A CRIU -j DROP\n" +
		"COMMIT\n"

	return ipTablesRestore(conf, n.netNS)
}

// Unlock removes all iptables rules in the network namespace.
func (n *iptablesLocker) Unlock(opts UnlockOptions) error {
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
