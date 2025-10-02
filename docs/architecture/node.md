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

### In-place Resource scaling

This makes use of the feature flag
[InPlacePodVerticalScaling](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)
to automatically update the pod resource requests to a minimum on scale down
events and revert them again on scale up. Once the Kubernetes feature flag is
enabled, it also needs to be enabled using the manager flag
`-in-place-scaling=true` plus some additional permissions are required for the
node driver to patch pods. To deploy this, simply uncomment the
`in-place-scaling` component in the `config/production/kustomization.yaml`.
This will add the flag and the required permissions when building the
kustomization.

### Status Labels

To reflect the container scaling status in the k8s API, the manager can set
status labels on a pod. This requires the flag `-status-labels=true`, which is
set by default in the production deployment.

The resulting labels have the following structure:

```yaml
status.zeropod.ctrox.dev/<container name>: <container status>
```

So if our pod has two containers, one of them running and one in scaled-down
state, the labels would be set like this:

```yaml
labels:
  status.zeropod.ctrox.dev/container1: RUNNING
  status.zeropod.ctrox.dev/container2: SCALED_DOWN
```

### Status Events

The manager can also be configured to emit Kubernetes events on scaling events
of a pod. This requires the flag `-status-events=true`, which is set by default
in the production deployment.

### All Flags

```
-debug
      enable debug logs
-in-place-scaling
      enable in-place resource scaling, requires InPlacePodVerticalScaling feature flag
-kubeconfig string
      Paths to a kubeconfig. Only required if out-of-cluster.
-metrics-addr string
      address of the metrics server (default ":8080")
-node-server-addr string
      address of the node server (default ":8090")
-probe-binary-name string
      set the probe binary name for probe detection (default "kubelet")
-status-events
      create status events to reflect container status
-status-labels
      update pod labels to reflect container status
-version
      output version and exit
```
