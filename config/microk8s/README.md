# MicroK8s Configuration

This directory contains the Kustomize configuration for deploying Zeropod on MicroK8s.

## Installation

```bash
kubectl apply -k config/microk8s
```

This applies the standard configuration with:
- Containerd socket path: `/run/containerd/containerd.sock`
- Containerd config path: `/var/snap/microk8s/current/args/containerd-template.toml`
- Namespace: `k8s.io`
- Host volume mounts adjusted for MicroK8s snap paths.

## Local Development and Testing

To test changes from a local clone of the repository without pushing to a remote registry, follow these steps:

### 1. Build Images
Build the installer and manager images locally using the `dev` tag.

```bash
make build-installer build-manager TAG=dev
```

### 2. Import Images into MicroK8s
MicroK8s uses its own containerd instance, so you must import the built images into it.

```bash
docker save ghcr.io/ctrox/zeropod-installer:dev | microk8s ctr image import -
docker save ghcr.io/ctrox/zeropod-manager:dev | microk8s ctr image import -
```

### 3. Update Kustomization
Temporarily update `config/microk8s/kustomization.yaml` to point to the local images and ensure `imagePullPolicy` allows local images (set to `IfNotPresent` or `Never`, as the default might be `Always` for tagged releases).

**Changes to `config/microk8s/kustomization.yaml`:**

```yaml
images:
- name: installer
  newName: ghcr.io/ctrox/zeropod-installer
  newTag: dev
- name: manager
  newName: ghcr.io/ctrox/zeropod-manager
  newTag: dev

patches:
  # ... existing patches ...
  - patch: |-
      # ... existing config patches ...
      # Add these lines to use local images
      - op: add
        path: /spec/template/spec/initContainers/0/imagePullPolicy
        value: IfNotPresent
      - op: add
        path: /spec/template/spec/containers/0/imagePullPolicy
        value: IfNotPresent
    target:
      kind: DaemonSet
```

### 4. Deploy
Apply the configuration to the cluster:

```bash
kubectl apply -k config/microk8s
```
