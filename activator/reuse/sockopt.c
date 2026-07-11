//go:build ignore

#include <linux/bpf.h>
#include <linux/socket.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>

char __license[] SEC("license") = "Dual MIT/GPL";

#define SOL_SOCKET 1
#define SO_REUSEPORT 15

SEC("cgroup/setsockopt")
int setsockopt(struct bpf_sockopt *ctx)
{
    struct bpf_sock *sk = ctx->sk;

    if (!sk)
        return 1;

    if (sk->protocol != IPPROTO_TCP)
        return 1;

    int reuseport_value = 1;
    // TODO:
    // * check what happens when SO_REUSEPORT is already set
    // * do we care about the return code?
    bpf_setsockopt(sk, SOL_SOCKET, SO_REUSEPORT, &reuseport_value, sizeof(reuseport_value));

    return 1;
}
