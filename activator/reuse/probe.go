package reuse

import (
	"fmt"
	"net"
	"os"

	"github.com/containernetworking/plugins/pkg/ns"
)

type probeListener struct {
	ln   *net.TCPListener
	lnFd *os.File
}

func (act *Activator) listenProbe(port uint16, network network, pl *probeListener) error {
	if err := act.ns.Do(func(nn ns.NetNS) error {
		ln, err := listenReuseport(port, network)
		if err != nil {
			return fmt.Errorf("wake listener: %w", err)
		}
		pl.ln = ln
		return nil
	}); err != nil {
		return err
	}
	return nil
}
