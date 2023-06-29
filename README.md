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

## How it works

First off, what is this containerd shim? The shim sits between containerd and
the container sandbox. Each container has such a long-running process that
calls out to runc to manage the lifecycle of a container.

![containerd architecture](https://github.com/containerd/containerd/blob/81bc6ce6e9f8f74af1bbbf25126db3b461cb0520/docs/cri/architecture.png)

There are several components that make zeropod work but here are the most
important ones:

* Checkpointing is done using [CRIU](https://github.com/checkpoint-restore/criu).
* After checkpointing a TCP socket is created in place of the application, the
  activator component is responsible for accepting a connection, closing the
  socket, restoring the process and then proxying the initial request(s) to
  the restored application.
* All subsequent connections go directly to the application without any
  proxying and performance impact.
* An eBPF trace is used to track the last TCP activity on the running
  application. This helps zeropod delay checkpointing if there is recent
  activity. This avoids too much flapping on a service that is frequently
  used.
* To the container runtime (e.g. Kubernetes), the container appears to be
  running even though the process is technically not. This is required to
  prevent the runtime from trying to restart the container.
* Metrics are recorded continuously within each shim and the zeropod-manager
  process that runs once per node (Daemonset) is responsible to collect and
  merge all metrics from the different shim processes. The shim exposes a unix
  socket for the manager to connect. The manager exposes the merged metrics on
  an HTTP endpoint.

## Use-cases

Only time will tell how useful zeropod actually is. Some made up use-cases
that could work are:

* Low traffic sites
* Dev/Staging environments
* "Mostly static" sites that still need some server component
* Hopefully more to be found

## Getting started

### Requirements

* Kubernetes v1.20+ (older versions with RuntimeClass enabled *might* work)
* Containerd

As zeropod is a runtime class, it needs to install binaries to `/opt/zeropod`
of your cluster nodes and also configure Containerd to load the shim. If you
first test this, it's probably best to use a [kind](https://kind.sigs.k8s.io)
cluster or something similar that you can quickly setup and delete again.

### Installing

Depending on your cluster setup, the predefined paths might not match your
containerd configuration. In this case you need to adjust the manifests in
config/node.yaml.

```bash
# install zeropod runtime and manager
kubectl apply -f https://github.com/ctrox/zeropod/blob/main/config/node.yaml
# create an example pod which makes use of zeropod
kubectl apply -f https://github.com/ctrox/zeropod/blob/main/config/examples/nginx.yaml
```

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
  annotations:
    # required, this is the port our to be scaled down application is listening on
    zeropod.ctrox.dev/port: "80"
    # required, this is the name of the container that will be scaled down
    zeropod.ctrox.dev/container-name: "nginx"
spec:
  runtimeClassName: zeropod
  containers:
    - name: nginx
      image: nginx
      ports:
        - containerPort: 80
```

Then there are a few annotations to tweak the behaviour of zeropod:

```yaml
# Configures long to wait before scaling down again after the last
# connnection. The duration is reset whenever a connection happens.
# Setting it to 0 means the application will be checkpointed as soon
# as possible after restore. Use with caution as this will cause lots
# of checkpoints/restores.
# Default is 1 minute.
zeropod.ctrox.dev/scaledownduration: 10s

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
format. The following metrics are currently available:

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
	- [ ] Support scaling more than 1 container in a zeropod (use-cases might be limited here)
- [x] Fix logs after restore
- [ ] Visibility into state (scaled down or up) from the outside
	- [ ] k8s events?
	- [x] Metrics
	- [ ] Custom resource that syncs with a zeropod?
- [x] Create installer DaemonSet (runtime shim, containerd config, criu binary)
- [x] e2e testing
- [x] Scale down/up on demand instead of time based
	- [x] technically scaling up on demand is now possible with exec. You can just exec a non-existing process.
- [ ] Less configuration - try to detect the port(s) a contanier is listening on
- [ ] Create uninstaller
- [ ] Configurable installer (paths etc.)

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
