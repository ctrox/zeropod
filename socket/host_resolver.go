package socket

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const hostProcPath = "/host/proc/"

// hostResolver uses the procfs of the host to resolve PIDs. With this the
// connection tracker can work when running in a container. As the ebpf
// program is not aware of the PID namespace that the processes running in, we
// need to find the PIDs of the host processes from the ones in the container.
type hostResolver struct{}

func (h hostResolver) Resolve(pid uint32) uint32 {
	p, err := findHostPid(hostProcPath, pid)
	if err != nil {
		return pid
	}

	return p
}

// findHostPid greps through the procfs to find the host pid of the supplied
// namespaced pid. It's very ugly but it works well enough for testing with
// Kind. It would be better to use the procfs package here but NSpid is always
// empty.
func findHostPid(procPath string, nsPid uint32) (uint32, error) {
	out, err := exec.Command("bash", "-c", fmt.Sprintf(`grep -P 'NSpid:.*\t%d\t' -ril %s*/status | head -n 1`, nsPid, procPath)).Output()
	if err != nil {
		return 0, err
	}

	strPid := strings.TrimSuffix(strings.TrimPrefix(string(out), procPath), "/status\n")
	pid, err := strconv.ParseUint(strPid, 10, 32)
	return uint32(pid), err
}
