# Configuration

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

## Annotations

The behaviour of zeropod can be adjusted with a number of pod annotations.

### Container Names

```yaml
zeropod.ctrox.dev/container-names: "nginx,sidecar"
```

A comma-separated list of container-names in the pod that should be considered
for scaling to zero. If unset or empty, all containers will be considered.

### Ports Map

```yaml
zeropod.ctrox.dev/ports-map: "nginx=80,81;sidecar=8080"
```

Ports-map configures the ports our to be scaled down application(s) are
listening on. As ports have to be matched with containers in a pod, the key is
the container name and the value a comma-delimited list of ports any TCP
connection on one of these ports will restore an application. If this annotation
is not specified, zeropod will try to find the listening ports automatically.
Use this option in case this fails for your application.

### Scale Down Duration

```yaml
zeropod.ctrox.dev/scaledown-duration: 10s
```

Configures how long to wait before scaling down again after the last connection.
The duration is reset whenever a new connection is detected. Setting it to 0
disables scaling down. If unset it defaults to 1 minute.

### Pre-dump

```yaml
zeropod.ctrox.dev/pre-dump: "true"
```

Execute a pre-dump before the full checkpoint and process stop. This can reduce
the checkpoint time in some cases but testing has shown that it also has a small
impact on restore time so YMMV. The default is false. See [the CRIU
docs](https://criu.org/Memory_changes_tracking) for details on what this does.

### Disable Checkpointing

```yaml
zeropod.ctrox.dev/disable-checkpointing: "true"
```

Disable checkpointing completely when scaling down. This option was introduced
for testing purposes to measure how fast some applications can be restored from
a complete restart instead of from memory images. If enabled, the process will
be killed on scale-down and all state is lost. This might be useful for some
use-cases where the application is stateless and super fast to startup.

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

Configure the buffer size of the probe detector. To be able to detect an HTTP
liveness/readiness probe, zeropod needs to read a certain amount of bytes from
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

## Experimental Features

Features that are marked as experimental might change form in the future or
could be removed entirely in future releases depending on the stability and
need.
