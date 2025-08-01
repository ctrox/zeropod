# zeropod - pod that scales down to zero

Zeropod is a Kubernetes runtime (more specifically a containerd shim) that
automatically checkpoints containers to disk after a certain amount of time of
the last TCP connection. While in scaled down state, it will listen on the same
port the application inside the container was listening on and will restore the
container on the first incoming connection. Depending on the memory size of the
checkpointed program this happens in tens to a few hundred milliseconds,
virtually unnoticable to the user. As all the memory contents are stored to disk
during checkpointing, all state of the application is restored. [It adjusts
resource requests](#in-place-resource-scaling) in scaled down state in-place if
the cluster supports it. To prevent huge resource usage spikes when draining a
node, scaled down pods can be [migrated between nodes](#zeropodctroxdevmigrate)
without needing to start up.

## Use-cases

Only time will tell how useful zeropod actually is. Some made up use-cases
that could work are:

* Low traffic sites
* Dev/Staging environments
* Providing a small tier on Heroku-like platforms
* "Mostly static" sites that still need some server component
* Hopefully more to be found

## How it works

First off, what is this containerd shim? The shim sits between containerd and
the container sandbox. Each container has such a long-running process that
calls out to runc to manage the lifecycle of a container.

<details><summary>show containerd architecture</summary>

![containerd architecture](https://github.com/containerd/containerd/blob/81bc6ce6e9f8f74af1bbbf25126db3b461cb0520/docs/cri/architecture.png)

</details>

There are several components that make zeropod work but here are the most
important ones:

* Checkpointing is done using [CRIU](https://github.com/checkpoint-restore/criu).
* After checkpointing, a userspace TCP proxy (activator) is created on a
  random port and an eBPF program is loaded to redirect packets destined to
  the checkpointed container to the activator. The activator then accepts the
  connection, restores the process, signals to disable the eBPF redirect and
  then proxies the initial request(s) to the restored application. See
  [activation sequence](#activation-sequence) for more details.
* All subsequent connections go directly to the application without any
  proxying and performance impact.
* An eBPF probe is used to track the last TCP activity on the running
  application. This helps zeropod delay checkpointing if there is recent
  activity. This avoids too much flapping on a service that is frequently
  used.
* To the container runtime (e.g. Kubernetes), the container appears to be
  running even though the process is technically not. This is required to
  prevent the runtime from trying to restart the container.
* When running `kubectl exec` on to the scaled down container, it will be
  restored and the exec should work just as with any normal Kubernetes
  container.
* Metrics are recorded continuously within each shim and the zeropod-manager
  process that runs once per node (DaemonSet) is responsible to collect and
  merge all metrics from the different shim processes. The shim exposes a unix
  socket for the manager to connect. The manager exposes the merged metrics on
  an HTTP endpoint.

### Activation sequence

This diagram shows what happens when a user initiates a connection to a
checkpointed container.

<details><summary>show diagram</summary>

```mermaid
sequenceDiagram
    actor User
    participant Redirector
    participant Activator
    participant Container
    Note over Container: checkpointed
    Note over Activator: listening on port 41234
    User->>Redirector: TCP connect to port 80
    Note right of User: local port 12345
    Redirector->>Redirector: redirect to port 41234
    Redirector->>Activator: TCP connect
    Activator->>Activator: TCP accept
    Activator->>Container: restore
    loop every millisecond
        Activator->>Container: TCP connect to port 80
    end
    Note over Container: restored
    Container-->>Activator: TCP accept
    Activator-->>Redirector: TCP accept
    Redirector-->>Redirector: redirect to port 12345
    Redirector-->>User: TCP accept
    Note right of User: connection between user<br>and container established
    User->>Container: TCP connect to port 80
    Note over Redirector: pass
    Container-->>User: TCP accept
    Note over Redirector: pass
```

</details>

## Compatibility

Most programs should to just work with zeropod out of the box. The
[examples](./config/examples) directory contains a variety of software that have been
tested successfully. If something fails, the containerd logs can prove useful
to figuring out what went wrong as it will output the CRIU log on
checkpoint/restore failure. What has proven somewhat flaky sometimes are some
arm64 workloads running in a linux VM on top of Mac OS. If you run into any
issues with your software, please don't hesitate to create an issue.

## Getting started

### Requirements

* Kubernetes v1.23+
* Containerd 1.6+

As zeropod is implemented using a [runtime
class](https://kubernetes.io/docs/concepts/containers/runtime-class/), it needs
to install binaries to your cluster nodes (by default in `/opt/zeropod`) and
also configure Containerd to load the shim. If you first test this, it's
probably best to use a [kind](https://kind.sigs.k8s.io) cluster or something
similar that you can quickly setup and delete again. It uses a DaemonSet called
`zeropod-node` for installing components on the node itself and also runs the
`manager` component for attaching the eBPF programs, collecting metrics and
facilitating migrations.

### Installation

The config directory comes with a few predefined manifests for use with
different Kubernetes distributions.

> ⚠️ The installer will restart the Containerd systemd service on each
> targeted node on the first install to load the config changes that are
> required for the zeropod shim to load. This is usually non-disruptive as
> Containerd is designed to be restarted without affecting any workloads.

```bash
# install zeropod runtime and manager
# "default" installation:
kubectl apply -k https://github.com/ctrox/zeropod/config/production

# GKE:
kubectl apply -k https://github.com/ctrox/zeropod/config/gke
```

> ⚠️⚠️⚠️ For k3s and rke2, the initial installation needs to restart the
> k3s/k3s-agent or rke2-server/rke2-agent services, since it's not possible to
> just restart Containerd. This will lead to restarts of other workloads on
> each targeted node.

```bash
# k3s:
kubectl apply -k https://github.com/ctrox/zeropod/config/k3s

# rke2:
kubectl apply -k https://github.com/ctrox/zeropod/config/rke2
```

By default, zeropod will only be installed on nodes with the label
`zeropod.ctrox.dev/node=true`. So after applying the manifest, label your
node(s) that should have it installed accordingly:

```bash
$ kubectl label node <node-name> zeropod.ctrox.dev/node=true
```

Once applied, check for `node` pod(s) in the `zeropod-system` namespace. If
everything worked it should be in status `Running`:

```bash
$ kubectl -n zeropod-system wait --for=condition=Ready pod -l app.kubernetes.io/name=zeropod-node
pod/zeropod-node-wgzrv condition met
```

Now you can create workloads which make use of zeropod.

```bash
# create an example pod which makes use of zeropod
kubectl apply -f https://github.com/ctrox/zeropod/config/examples/nginx.yaml
```

Depending on your cluster setup, none of the predefined configs might not
match yours. In this case you need clone the repo and adjust the manifests in
`config/` accordingly. If your setup is common, a PR to add your configuration
adjustments would be most welcome.

### Uninstalling

To uninstall zeropod, you can apply the uninstall manifest to spawn a pod to
do the cleanup on all labelled zeropod nodes. After all the uninstall pods
have finished, we can delete all the manifests.

```bash
kubectl apply -k https://github.com/ctrox/zeropod/config/uninstall
kubectl -n zeropod-system wait --for=condition=Ready pod -l app.kubernetes.io/name=zeropod-node
kubectl delete -k https://github.com/ctrox/zeropod/config/production
```

## Configuration

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

### Probes

Zeropod is able to intercept liveness probes while the container process is
scaled down to ensure the application is not restored for probes. This just
works for HTTP and TCP probes, GRPC and exec probes will wake the container up.

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
also customize the path and the headers of the probe, just be mindful of the
size of those. To reduce memory usage, by default, zeropod will only read the
first `1024` bytes of each request to detect an HTTP probe. If the probe is
larger than that, traffic will just be passed through and the app will be
restored on each probe request. In that case, it can be increased with the
[probe buffer size](#zeropodctroxdevprobe-buffer-size) annotation.

### `zeropod.ctrox.dev/container-names`

A comma-separated list of container-names in the pod that should be considered
for scaling to zero. If unset or empty, all containers will be considered.

```yaml
zeropod.ctrox.dev/container-names: "nginx,sidecar"
```

### `zeropod.ctrox.dev/ports-map`

Ports-map configures the ports our to be scaled down application(s) are
listening on. As ports have to be matched with containers in a pod, the key is
the container name and the value a comma-delimited list of ports any TCP
connection on one of these ports will restore an application. If omitted, the
zeropod will try to find the listening ports automatically, use this option in
case this fails for your application.

```yaml
zeropod.ctrox.dev/ports-map: "nginx=80,81;sidecar=8080"
```

### `zeropod.ctrox.dev/scaledown-duration`

Configures how long to wait before scaling down again after the last
connnection. The duration is reset whenever a connection happens. Setting it to
0 disables scaling down. If unset it defaults to 1 minute.

```yaml
zeropod.ctrox.dev/scaledown-duration: 10s
```

### `zeropod.ctrox.dev/pre-dump`

Execute a pre-dump before the full checkpoint and process stop. This can reduce
the checkpoint time in some cases but testing has shown that it also has a small
impact on restore time so YMMV. The default is false. See
https://criu.org/Memory_changes_tracking for details on what this does.

```yaml
zeropod.ctrox.dev/pre-dump: "true"
```

### `zeropod.ctrox.dev/disable-checkpointing`

Disable checkpointing completely. This option was introduced for testing
purposes to measure how fast some applications can be restored from a complete
restart instead of from memory images. If enabled, the process will be killed on
scale-down and all state is lost. This might be useful for some use-cases where
the application is stateless and super fast to startup.

```yaml
zeropod.ctrox.dev/disable-checkpointing: "true"
```

### `zeropod.ctrox.dev/disable-probe-detection`

Disables the probe detection mechanism. If there are probes defined on a
container, they will be forwarded to the container just like any traffic and
will wake it up.

### `zeropod.ctrox.dev/probe-buffer-size`

Configure the buffer size of the probe detector. To be able to detect an HTTP
liveness/readiness probe, zeropod needs to read a certain amount of bytes from
the TCP stream of incoming connections. This normally does not need to be
adjusted as the default should fit most probes and only needs to be increased in
case the probe contains lots of header data. Defaults to `1024` if unset.

## Experimental features

### `zeropod.ctrox.dev/migrate`

Enables migration of scaled down containers by listing the containers to be
migrated. When such an annotated pod is deleted and it's part of a Deployment,
the new pod will fetch the checkpoints of these containers and instead of
starting it will simply wait for activation again. This minmizes the surge in
resources if for example a whole node of scaled down zeropods is drained.

```yaml
zeropod.ctrox.dev/migrate: "nginx,sidecar"
```

### `zeropod.ctrox.dev/live-migrate`

Enables live-migration of a running container in the pod. Only one container per
pod is supported at this point. When such an annotated pod is deleted and it's
part of a Deployment, the new pod will do a lazy-migration of the memory
contents. This requires
[userfaultd](https://www.kernel.org/doc/html/latest/admin-guide/mm/userfaultfd.html)
to be enabled in the host kernel (`CONFIG_USERFAULTFD`).

```yaml
zeropod.ctrox.dev/live-migrate: "nginx"
```

### `io.containerd.runc.v2.group`

It's possible to reduce the resource usage further by grouping multiple pods
into one shim process. The value of the annotation specifies the group id, each
of which will result in a shim process. This is currently marked as experimental
since not much testing has been done and new issues might surface when using
grouping.

```yaml
io.containerd.runc.v2.group: "zeropod"
```

## zeropod-node

The zeropod-node Daemonset is scheduled on every node labelled
`zeropod.ctrox.dev/node=true`. The individual components of the node daemon
are documented in this section.

### Installer

The installer runs as an init-container and runs the binary
`cmd/installer/main.go` with some distro-specific options to install the
runtime binaries, configure containerd and register the `RuntimeClass`.

### Manager

The manager component starts after the installer init-container has succeeded.
It provides functionality that is needed on a node-level and is would bloat
the shim otherwise. For example, loading eBPF programs can be quite memory
intensive so they have been moved from the shim to the manager to keep the
shim memory usage as minimal as possible.

These are the responsibilities of the manager:

- Loading eBPF programs that the shim(s) rely on.
- Collect metrics from all shim processes and expose them on HTTP for scraping.
- Subscribes to shim scaling events and adjusts Pod requests.

#### In-place Resource scaling

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

#### Status Labels

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

#### Flags

```
-metrics-addr=":8080"    sets the address of the metrics server
-debug                   enables debug logging
-in-place-scaling=false  enable in-place resource scaling, requires InPlacePodVerticalScaling feature flag
-status-labels=false     update pod labels to reflect container status
```

## Metrics

The zeropod-node pod exposes metrics on `0.0.0.0:8080/metrics` in Prometheus
format on each installed node. The metrics address can be configured with the
`-metrics-addr` flag. The following metrics are currently available:

```bash
# HELP zeropod_checkpoint_duration_seconds The duration of the last checkpoint in seconds.
# TYPE zeropod_checkpoint_duration_seconds histogram
zeropod_checkpoint_duration_seconds_bucket{container="nginx",namespace="default",pod="nginx",le="+Inf"} 3
zeropod_checkpoint_duration_seconds_sum{container="nginx",namespace="default",pod="nginx"} 0.749254206
zeropod_checkpoint_duration_seconds_count{container="nginx",namespace="default",pod="nginx"} 3
# HELP zeropod_last_checkpoint_time A unix timestamp in nanoseconds of the last checkpoint.
# TYPE zeropod_last_checkpoint_time gauge
zeropod_last_checkpoint_time{container="nginx",namespace="default",pod="nginx"} 1.688065891505882e+18
# HELP zeropod_last_restore_time A unix timestamp in nanoseconds of the last restore.
# TYPE zeropod_last_restore_time gauge
zeropod_last_restore_time{container="nginx",namespace="default",pod="nginx"} 1.688065880496497e+18
# HELP zeropod_restore_duration_seconds The duration of the last restore in seconds.
# TYPE zeropod_restore_duration_seconds histogram
zeropod_restore_duration_seconds_bucket{container="nginx",namespace="default",pod="nginx",le="+Inf"} 4
zeropod_restore_duration_seconds_sum{container="nginx",namespace="default",pod="nginx"} 0.684013211
zeropod_restore_duration_seconds_count{container="nginx",namespace="default",pod="nginx"} 4
# HELP zeropod_running Reports if the process is currently running or checkpointed.
# TYPE zeropod_running gauge
zeropod_running{container="nginx",namespace="default",pod="nginx"} 0
```

## Development

For iterating on shim development it's recommended to use
[kind](https://kind.sigs.k8s.io). Once installed and a cluster has been
created (`kind create cluster --config=e2e/kind.yaml`) run `make install-kind`
to build and install everything on the kind cluster. After making code changes
the fastest way to update the shim is using `make build-kind`, since this will
only build the binary and copy the updated binary to the cluster.

### Developing on an M1+ Mac

It can be a bit hard to get this running on an arm Mac. First off, the shim
itself does not run on MacOS at all as it requires linux. But we can run it
inside a kind cluster using a podman machine. One important thing to note is
that the podman machine needs to run rootful, else checkpointing (CRIU) does
not seem to work. Also so far I have not been able to get this running with
Docker desktop.

Dependencies:

* [podman](https://podman.io/docs/installation#macos)
* [kind](https://kind.sigs.k8s.io/docs/user/quick-start#installing-with-a-package-manager)

```bash
podman machine init --rootful
podman machine start
kind create cluster --config=e2e/kind.yaml
make install-kind
```

Now your kind cluster should have a working zeropod installation. The e2e
tests can also be run but it's a bit more involved than just running `go test`
since that requires `GOOS=linux`. You can use `make docker-test-e2e` to run
the e2e tests within a docker container, so everything will be run on the
linux podman VM.
