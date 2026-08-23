# Configuration

> [!IMPORTANT]
> Features/Flags that are marked as experimental might change form in the future
> or could be removed entirely in future releases depending on stability and
> need.

A pod can make use of zeropod only if the `runtimeClassName` is set to
`zeropod`. See this minimal example of a pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginx
  labels:
    app: nginx
spec:
  runtimeClassName: zeropod
  containers:
    - name: nginx
      image: nginx
      ports:
        - containerPort: 80
```

## Probes

Zeropod is able to intercept liveness probes while the container process is
scaled down to ensure the application is not restored for probes. This works
only for HTTP and TCP probes; gRPC and exec probes will wake the container up.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginx
  annotations:
    zeropod.ctrox.dev/scaledown-duration: 10s
spec:
  runtimeClassName: zeropod
  containers:
    - name: nginx
      image: nginx
      ports:
        - containerPort: 80
      livenessProbe:
        httpGet:
          port: 80
```

In this example, the container will be scaled down 10 seconds after starting
even though we have defined a probe. Zeropod will take care of replying to the
probe when the container is scaled down. Whenever the container is running, the
probe traffic will be forwarded to the app just like normal traffic. You can
also customize the path and headers of the probe; just be mindful of their
size. To reduce memory usage, by default, zeropod will only read the
first `1024` bytes of each request to detect an HTTP probe. If the probe is
larger than that, traffic will just be passed through and the app will be
restored on each probe request. In that case, the limit can be increased with the
[probe buffer size](#zeropodctroxdevprobe-buffer-size) annotation.

## Annotations

The behavior of zeropod can be adjusted with a number of pod annotations.

### Container Names

```yaml
zeropod.ctrox.dev/container-names: "nginx,sidecar"
```

A comma-separated list of container names in the pod that should be considered
for scaling to zero. If unset or empty, all containers will be considered.

### Ports Map

```yaml
zeropod.ctrox.dev/ports-map: "nginx=80,81;sidecar=8080"
```

Ports-map configures the ports our to-be-scaled-down application(s) are
listening on. As ports have to be matched with containers in a pod, the key is
the container name and the value is a comma-delimited list of ports. Any TCP
connection on one of these ports will restore an application. If this annotation
is not specified, zeropod will try to find the listening ports automatically.
Use this option in case this fails for your application.

### Scale Down Duration

```yaml
zeropod.ctrox.dev/scaledown-duration: 10s
```

Configures how long to wait before scaling down again after the last connection.
The duration is reset whenever a new connection is detected. Setting it to `0`
disables scaling down. If unset, it defaults to 1 minute.

### Pre-dump

```yaml
zeropod.ctrox.dev/pre-dump: "true"
```

Executes a pre-dump before the full checkpoint and process stop. This can reduce
the checkpoint time in some cases, but testing has shown that it also has a small
impact on restore time, so YMMV. The default is `false`. See [the CRIU
docs](https://criu.org/Memory_changes_tracking) for details on what this does.

### Disable Checkpointing

```yaml
zeropod.ctrox.dev/disable-checkpointing: "true"
```

Disables checkpointing completely when scaling down. This option was introduced
for testing purposes to measure how fast some applications can be restored from
a complete restart instead of from memory images. If enabled, the process will
be killed on scale-down and all state is lost. This might be useful for some
use cases where the application is stateless and super fast to start up.

### Disable Probe Detection

```yaml
zeropod.ctrox.dev/disable-probe-detection: "true"
```

Disables the [probe detection mechanism](#probes). If there are probes defined
on a container, they will be forwarded to the container just like any traffic
and will wake it up.

### Probe Buffer Size

```yaml
zeropod.ctrox.dev/probe-buffer-size: "1024"
```

Configures the buffer size of the probe detector. To be able to detect an HTTP
liveness/readiness probe, zeropod needs to read a certain number of bytes from
the TCP stream of incoming connections. This normally does not need to be
adjusted as the default should fit most probes and only needs to be increased in
case the probe contains lots of header data. Defaults to `1024` if unset.

### Connect Timeout

```yaml
zeropod.ctrox.dev/connect-timeout: "5s"
```

Configures how long to wait for the container process to restore when proxying
the initial connections. Defaults to `5s` if unset.

### Proxy Timeout

```yaml
zeropod.ctrox.dev/proxy-timeout: "5s"
```

Configures how long to proxy a connection to the container process after it has
been established. Defaults to `5s` if unset.

## Manager/Installer Configuration

Some options can be configured on a node level instead of per pod. These are
exposed as flags on the zeropod-node DaemonSet, so if you just run one version of
the DaemonSet these settings will apply cluster-wide. Some of these settings can
also be toggled with their respective kustomize components.

### General Configuration

* **`-tracker-ignore-localhost`** (default `true`, component `installer`): Tells
  the tracker to ignore traffic from localhost. Useful if a sidecar connects to
  the zeropod container and should not cause a scale-down delay. Note that
  localhost traffic will still cause the container to restore if it's already
  scaled down.

### In-Place Resource Scaling

This makes use of
[InPlacePodVerticalScaling](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)
to automatically update the pod resource requests to a minimum on scale-down
events and revert them again on scale-up. This has been enabled by default since
Kubernetes v1.33 and is enabled by default when using the `production` kustomize
overlay.

* **`-in-place-scaling`** (default `false`, component `manager`): Enables
  in-place pod resource updates.

### Status Labels

To reflect the container scaling status in the Kubernetes API, the manager can set
status labels on a pod. This is enabled by default when using the `production`
kustomize overlay.

The resulting labels have the following structure:

```yaml
status.zeropod.ctrox.dev/<container name>: <container status>
```

So if our pod has two containers, one of them running and one in a scaled-down
state, the labels would be set like this:

```yaml
labels:
  status.zeropod.ctrox.dev/container1: RUNNING
  status.zeropod.ctrox.dev/container2: SCALED_DOWN
```

* **`-status-labels`** (default `false`, component `manager`): Updates pod labels
  to reflect container scaling status.

### Status Events

The manager can also be configured to emit Kubernetes events on scaling events
of a pod. This is enabled by default when using the `production` kustomize
overlay.

* **`-status-events`** (default `false`, component `manager`): Creates status
  events to reflect container scaling status.

### Capacity Eviction

Capacity eviction prevents node out-of-memory scenarios by ensuring the node has
enough resources to restore a container before allowing it to happen. If the
node does not have enough capacity, it will evict the requesting pod and forward
requests to the new one as soon as it's available.

* **`-capacity-request`** (default `false`, component `installer`): Enables the
  shim to make a capacity request before restoring a container to ensure the
  node has enough resources. If the node does not have enough capacity, it will
  evict the pod and forward requests to the new one as soon as it's available.
* **`-capacity-eviction-threshold`** (default `1.0`, component `manager`): The
  memory threshold ratio when capacity eviction starts. Values above `1.0` allow
  intentional overprovisioning.
* **`-capacity-system-memory`** (default `false`, component `manager`): Uses
  system memory usage instead of individual pod memory requests for capacity
  tracking.
* **`-capacity-eviction-timeout`** (default `1m`, component `manager`): How long
  to wait for a pod eviction operation to complete before timing out.

### Migration Settings

* **`-pages-transfer-timeout`** (default `5m`, component `manager`): How long to
  wait for memory pages transfer during live migration.
* **`-migration-servers-timeout`** (default `10s`, component `manager`): How long
  to wait for target migration servers to start.
* **`-migration-claim-timeout`** (default `10s`, component `manager`): How long
  to wait for a migration task to be claimed by the controller. Pods without a
  target will be in `Terminating` state for the duration of this timeout.
* **`-migration-ready-timeout`** (default `5m`, component `manager`): How long to
  wait for the target pod to reach the ready-for-restoring state.
* **`-auto-gc-migrations`** (default `true`, component `manager`): Automatically
  garbage collects migration resources when the owning pod is deleted.

### Reuseport Activator (Experimental)

This enables a completely new activator which handles traffic to the container
purely in the kernel and requires no userspace proxying. See [the architecture
document](../architecture/reuseport.md) for more details on how this works.

> [!WARNING]
> Requires Linux kernel 6.6 or later. Eventually this aims to be the default
> activator but until then proceed with caution as this has not yet been tested
> on a large variety of systems.

* **`-reuseport-activator`** (default `false`, component `installer`): Enables
  the new reuseport activator.
