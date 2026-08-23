# Node

The zeropod-node Daemonset is scheduled on every node labelled
`zeropod.ctrox.dev/node=true`. The individual components of the node daemon
are documented in this section.

## Installer

The installer runs as an init-container and runs the binary
`cmd/installer/main.go` with some distro-specific options to install the
runtime binaries, configure containerd and register the `RuntimeClass`.

## Manager

The manager component starts after the installer init-container has succeeded.
It provides functionality that is needed on a node-level and is would bloat
the shim otherwise. For example, loading eBPF programs can be quite memory
intensive so they have been moved from the shim to the manager to keep the
shim memory usage as minimal as possible.

These are the responsibilities of the manager:

- Loading eBPF programs that the shim(s) rely on.
- Collect metrics from all shim processes and expose them on HTTP for scraping.
- Subscribes to shim scaling events and adjusts Pod requests.

## Configuration

The various config flags the installer/manager provide are documented in the
relevant [configuration
section](../configuration/README.md#managerinstaller-configuration).
