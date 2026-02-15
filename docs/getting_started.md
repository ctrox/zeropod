# Getting started

## Requirements

* Kubernetes v1.23+
* Containerd 1.7+

As zeropod is implemented using a [runtime
class](https://kubernetes.io/docs/concepts/containers/runtime-class/), it needs
to install binaries to your cluster nodes (by default in `/opt/zeropod`) and
also configure Containerd to load the shim. If you first test this, it's
probably best to use a [kind](https://kind.sigs.k8s.io) cluster or something
similar that you can quickly setup and delete again. It uses a DaemonSet called
`zeropod-node` for installing components on the node itself and also runs the
`manager` component for attaching the eBPF programs, collecting metrics and
facilitating migrations.

## Installation

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

> ⚠️⚠️⚠️ For k3s, rke2 and microk8s, the initial installation needs to restart the
> k3s/k3s-agent, rke2-server/rke2-agent or snap services, since it's not possible to
> just restart Containerd. This might lead to restarts of other workloads on
> each targeted node depending on the k3s/rke2/microk8s version.

```bash
# k3s:
kubectl apply -k https://github.com/ctrox/zeropod/config/k3s

# rke2:
kubectl apply -k https://github.com/ctrox/zeropod/config/rke2

# microk8s:
kubectl apply -k https://github.com/ctrox/zeropod/config/microk8s
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
kubectl apply -f https://raw.githubusercontent.com/ctrox/zeropod/refs/heads/main/config/examples/nginx.yaml
```

### Verifying Checkpoint and Restore

The nginx example has a `scaledown-duration` of 10 seconds. After no external
traffic for 10 seconds, the container will be checkpointed (frozen) to disk.

**1. Watch the manager logs:**
```bash
kubectl logs -f -n zeropod-system -l app.kubernetes.io/name=zeropod-node -c manager
```

Look for status events:
- `"phase":1` means RUNNING
- `"phase":0` means SCALED_DOWN (checkpointed)

**2. Trigger a restore:**

Once you see `phase:0`, the container is checkpointed. Send a request to wake it:
```bash
POD_IP=$(kubectl get pod -l app=nginx -o jsonpath='{.items[0].status.podIP}')
curl $POD_IP
```

The manager logs should show `phase:1` again, indicating the container was
restored from checkpoint. The curl request will succeed as soon as the
container is ready.

**3. Verify checkpoint data on disk:**
```bash
sudo ls -la /var/lib/zeropod/i/
```

Depending on your cluster setup, none of the predefined configs might not
match yours. In this case you need clone the repo and adjust the manifests in
`config/` accordingly. If your setup is common, a PR to add your configuration
adjustments would be most welcome.

## Uninstalling

To uninstall zeropod, you can apply the uninstall manifest to spawn a pod to
do the cleanup on all labelled zeropod nodes. After all the uninstall pods
have finished, we can delete all the manifests.

```bash
kubectl apply -k https://github.com/ctrox/zeropod/config/uninstall
kubectl -n zeropod-system wait --for=condition=Ready pod -l app.kubernetes.io/name=zeropod-node
kubectl delete -k https://github.com/ctrox/zeropod/config/production
```
