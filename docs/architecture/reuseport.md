# Reuseport Activator

Since the beginning, zeropod used a small Go proxy that would listen on a random
free port in the network namespace of the container to be able to accept and
buffer traffic while the application was scaled down. Traffic destined to the
container's actual port was redirected by an eBPF TC program depending on
the scaling state of the container. This meant the initial traffic that woke the
container up was routed to the Go proxy to toggle the restore and let it sit
there in an accepted state. Once the container was up and running, the eBPF program
would switch to directly pass traffic to the restored application, bypassing the
proxy again.

This creates a few problems:

* To the application, the initial traffic appears to be coming from localhost.
* Eventually these initial connections were being closed by zeropod in order
  not to proxy them indefinitely. This causes connection resets for applications
  that rely on long-running connections.
* Proxying has a certain overhead. Although this is limited to the initial
  traffic burst, it's still there.
* There's quite a bit of complexity in making the switch from proxying to the
  direct connection work (connection tracking, etc.).

These are all solved with the new [`BPF_PROG_TYPE_SK_REUSEPORT`
based](https://docs.ebpf.io/linux/program-type/BPF_PROG_TYPE_SK_REUSEPORT)
approach.

Before the container application is started, it attaches an eBPF program which
will enforce the `SO_REUSEPORT` option on every listener that is created in the
container's cgroup. This option allows multiple sockets to occupy the same
listening port. The sockets don't even need to belong to the same process. The
eBPF program is there to steer connections to the different sockets depending on
the container state. Let's go into detail about how that works.

The eBPF program uses a map
[BPF_MAP_TYPE_REUSEPORT_SOCKARRAY](https://docs.ebpf.io/linux/map-type/BPF_MAP_TYPE_REUSEPORT_SOCKARRAY/)
to store different listening sockets belonging to the same group. In our case, we
have 3 keys in this map:

* `0` - the app listener. Populated only when the application is running.
* `1` - the wake listener. Used for detecting when to wake the application.
* `2` - the probe listener. Used for intercepting kubelet probes.

On container start, the activator detects the listening sockets of the
application. It then puts these into slot `0` of an instance of the
aforementioned map. It also creates two additional listeners per app listener:

1. Wake Listener

The purpose of this listener is to detect and react to incoming traffic when the
container is scaled down. It won't ever call `accept` on an incoming connection;
it's just there being polled by the activator. Once it detects a connection, it
triggers the restore process of zeropod (CRIU restore or container start
depending on the mode). Once the application is restored, we find the app's
listening socket(s) and put them into the app slot of the BPF map. To complete
the transition, the only thing the wake socket needs to do is close itself. The
kernel will take care of [migrating all pending
connections](https://docs.ebpf.io/linux/program-type/BPF_PROG_TYPE_SK_REUSEPORT/#socket-migration)
to the actual app socket. To the initiating side of the TCP connection, this is
completely transparent. Also for the restored application, traffic will appear
to be coming directly from the initiating client IP. Once the app is being scaled
down again, the same happens in reverse. Pending connections that just came in
while it was being checkpointed will go to the wake listener and the whole cycle
starts from scratch.

2. Probe Listener

This is used to handle Kubernetes TCP/HTTP probes when the container is scaled
down. Probe connections are detected by the source address belonging to
kubelet, and the `BPF_PROG_TYPE_SK_REUSEPORT` program redirects them accordingly.
Incoming requests to this listener are accepted by the Go program and will be
replied to with a very minimal HTTP response that satisfies kubelet:

```
HTTP/1.1 200 OK
Server: zeropod probe
Connection: close

ok
```

The eBPF program always prefers to route traffic to the app listener. If that's
not there, it will route to the wake listener *unless* the source IP matches
the kubelet IP; then it will be directed to the probe listener. This makes the
whole thing way simpler to understand compared to the rather convoluted old
implementation while solving all of the stated issues.
