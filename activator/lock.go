package activator

import (
	"strconv"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// LockNetwork "locks" the network by adding a nftables rule that drops all
// incoming traffic in the specified network namespace.
func LockNetwork(netNS ns.NetNS) error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(netNS.Fd())))
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

// UnlockNetwork removes the netfilter table created by LockNetwork.
// Additionally it also removes any CRIU-created tables if the pid of the
// restored process is given.
func UnlockNetwork(netNS ns.NetNS, pid int) error {
	nft, err := nftables.New(nftables.WithNetNSFd(int(netNS.Fd())))
	if err != nil {
		return err
	}
	defer nft.CloseLasting()

	if pid != 0 {
		nft.DelTable(&nftables.Table{Name: "CRIU-" + strconv.Itoa(pid), Family: nftables.TableFamilyINet})
	}

	nft.DelTable(&nftables.Table{Name: "zeropod", Family: nftables.TableFamilyINet})

	return nft.Flush()
}
