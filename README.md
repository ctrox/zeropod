# zeropod - pod that scales down to zero

Zeropod is a Kubernetes runtime (more specifically a containerd shim) that
automatically checkpoints containers to disk after a certain amount of time of
the last TCP connection. While in scaled down state, it will listen on the
same port the application inside the container was listening on and will
restore the container on the first incoming connection. Depending on the
memory size of the checkpointed program this happens in tens to a few hundred
milliseconds, virtually unnoticable to the user. As all the memory contents
are stored to disk during checkpointing, all state of the application is
restored.

## Use-cases

Only time will tell how useful zeropod actually is. Some made up use-cases
that could work are:

* Low traffic sites
* Dev/Staging environments
* "Mostly static" sites that still need some server component
* Hopefully more to be found

## How it works

First off, what is this containerd shim? The shim sits between containerd and
the container sandbox. Each container has such a long-running process that
calls out to runc to manage the lifecycle of a container.

![containerd architecture](https://github.com/containerd/containerd/blob/81bc6ce6e9f8f74af1bbbf25126db3b461cb0520/docs/cri/architecture.png)

There are several components that make zeropod work but here are the most
important ones:

* Checkpointing is done using [CRIU](https://github.com/checkpoint-restore/criu).
* After checkpointing, a TCP socket is created in place of the application.
  The activator component is responsible for accepting a connection, closing
  the socket, restoring the process and then proxying the initial request(s)
  to the restored application.
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

## Compatibility

Most programs should to just work with zeropod out of the box. The
[examples](./examples) directory contains a variety of software that have been
tested successfully. If something fails, the containerd logs can prove useful
to figuring out what went wrong as it will output the CRIU log on
checkpoint/restore failure. What has proven somewhat flaky sometimes are some
arm64 workloads running in a linux VM on top of Mac OS. If you run into any
issues with your software, please don't hesitate to create an issue.

## Getting started

### Requirements

* Kubernetes v1.20+ (older versions with RuntimeClass enabled *might* work)
* Containerd (1.5+ has been tested)

As zeropod is implemented using a [runtime
class](https://kubernetes.io/docs/concepts/containers/runtime-class/), it
needs to install binaries to your cluster nodes (by default in `/opt/zeropod`)
and also configure Containerd to load the shim. If you first test this, it's
probably best to use a [kind](https://kind.sigs.k8s.io) cluster or something
similar that you can quickly setup and delete again. It uses a DaemonSet for
installing components on the node itself and also runs a `manager` component
as a second container for collecting metrics and probably other uses in the
future.

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
$ kubectl get pods -n zeropod-system
NAME                 READY   STATUS    RESTARTS   AGE
zeropod-node-zr8k7   2/2     Running   0          35s
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

## Configuration

A pod can make use of zeropod only if the `runtimeClassName` is set to
`zeropod`. Apart from that there are two annotations that are currently
required. See this minimal example of a pod:

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

Then there are also a few optional annotations that can be set on the pod to
tweak the behaviour of zeropod:

```yaml
# container-names of containers in the pod that should be considered for
# scaling to zero. If empty all containers will be considered.
zeropod.ctrox.dev/container-names: "nginx,sidecar"

# ports-map configures the ports our to be scaled down application(s) are
# listening on. As ports have to be matched with containers in a pod, the
# key is the container name and the value a comma-delimited list of ports
# any TCP connection on one of these ports will restore an application.
# If omitted, the zeropod will try to find the listening ports automatically,
# use this option in case this fails for your application.
zeropod.ctrox.dev/ports-map: "nginx=80,81;sidecar=8080"

# Configures long to wait before scaling down again after the last
# connnection. The duration is reset whenever a connection happens.
# Setting it to 0 means the application will be checkpointed as soon
# as possible after restore. Use with caution as this will cause lots
# of checkpoints/restores.
# Default is 1 minute.
zeropod.ctrox.dev/scaledown-duration: 10s

# Execute a pre-dump before the full checkpoint and process stop. This can
# reduce the checkpoint time in some cases but testing has shown that it also
# has a small impact on restore time so YMMV. The default is false.
# See https://criu.org/Memory_changes_tracking for details on what this does.
zeropod.ctrox.dev/pre-dump: "true"

# Disable checkpointing completely. This option was introduced for testing
# purposes to measure how fast some applications can be restored from a complete
# restart instead of from memory images. If enabled, the process will be
# killed on scale-down and all state is lost. This might be useful for some
# use-cases where the application is stateless and super fast to startup.
zeropod.ctrox.dev/disable-checkpointing: "true"
```

## Metrics

The zeropod-node pod exposes metrics on `0.0.0.0:8080/metrics` in Prometheus
format on each installed node. The following metrics are currently available:

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

## TODO

- [x] Support more than 1 container in a zeropod
	- [x] Support scaling more than 1 container in a zeropod (use-cases might be limited here)
- [x] Fix logs after restore
- [ ] Visibility into state (scaled down or up) from the outside
	- [ ] k8s events?
	- [x] Metrics
	- [ ] Custom resource that syncs with a zeropod?
- [x] Create installer DaemonSet (runtime shim, containerd config, criu binary)
- [x] e2e testing
- [x] Scale down/up on demand instead of time based
	- [x] technically scaling up on demand is now possible with exec. You can just exec a non-existing process.
- [x] Less configuration - try to detect the port(s) a contanier is listening on
- [x] Configurable installer (paths etc.)
- [ ] Create uninstaller
- [ ] Create issues instead of this list

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
