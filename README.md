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
