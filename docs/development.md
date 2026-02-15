# Development

For iterating on shim development it's recommended to use
[kind](https://kind.sigs.k8s.io). Once installed and a cluster has been
created (`kind create cluster --config=e2e/kind.yaml`) run `make install-kind`
to build and install everything on the kind cluster. After making code changes
the fastest way to update the shim is using `make build-kind`, since this will
only build the binary and copy the updated binary to the cluster.

## Developing on an M1+ Mac

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

## Developing on MicroK8s

To test changes from a local clone on MicroK8s:

### 1. Build and Import Images

```bash
make build-installer build-manager TAG=dev
docker save ghcr.io/ctrox/zeropod-installer:dev | microk8s ctr image import -
docker save ghcr.io/ctrox/zeropod-manager:dev | microk8s ctr image import -
```

### 2. Update Kustomization

Temporarily update `config/microk8s/kustomization.yaml` for local development:

```yaml
resources:
  - ../production
images:
- name: ghcr.io/ctrox/zeropod-installer
  newTag: dev
- name: ghcr.io/ctrox/zeropod-manager
  newTag: dev
patches:
  - path: microk8s.yaml
  - patch: |-
      - op: add
        path: /spec/template/spec/initContainers/0/args
        value: []
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -runtime=microk8s
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -systemd-cgroup=false
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -containerd-socket=/run/containerd/containerd.sock
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -containerd-config=/etc/containerd/containerd-template.toml
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -containerd-namespace=k8s.io
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: -probe-binary-name=kubelite
    target:
      kind: DaemonSet
  - patch: |-
      - op: add
        path: /spec/template/spec/initContainers/0/imagePullPolicy
        value: IfNotPresent
      - op: add
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: IfNotPresent
    target:
      kind: DaemonSet
```

### 3. Deploy

```bash
kubectl apply -k config/microk8s
```

## Developing on K3s

To test changes from a local clone on K3s:

### 1. Build and Import Images

```bash
make build-installer build-manager TAG=dev
docker save ghcr.io/ctrox/zeropod-installer:dev | sudo k3s ctr images import -
docker save ghcr.io/ctrox/zeropod-manager:dev | sudo k3s ctr images import -
```

### 2. Update Kustomization

Temporarily update `config/k3s/kustomization.yaml` for local development:

```yaml
resources:
  - ../production
images:
- name: ghcr.io/ctrox/zeropod-installer
  newTag: dev
- name: ghcr.io/ctrox/zeropod-manager
  newTag: dev
patches:
  - path: k3s.yaml
  - patch: |-
      - op: add
        path: /spec/template/spec/initContainers/0/args/-
        value: -runtime=k3s
    target:
      kind: DaemonSet
  - patch: |-
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: -probe-binary-name=k3s
    target:
      kind: DaemonSet
  - patch: |-
      - op: add
        path: /spec/template/spec/initContainers/0/imagePullPolicy
        value: IfNotPresent
      - op: add
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: IfNotPresent
    target:
      kind: DaemonSet
```

### 3. Deploy

```bash
sudo k3s kubectl apply -k config/k3s
```
