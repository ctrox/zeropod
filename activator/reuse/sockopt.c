//go:build ignore

#include <linux/bpf.h>
#include <linux/socket.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>

char __license[] SEC("license") = "Dual MIT/GPL";

#define SOL_SOCKET     1
#define SO_REUSEPORT   15
#define SOL_IPV6       41
#define IPV6_V6ONLY    26

SEC("cgroup/setsockopt")
int setsockopt(struct bpf_sockopt *ctx)
{
    int *optval = ctx->optval;
    struct bpf_sock *sk = ctx->sk;

    if (!optval || (void *)(optval + 1) > ctx->optval_end) {
        return 1;
    }

    if (!sk)
        return 1;

    if (sk->protocol != IPPROTO_TCP)
        return 1;

    int reuseport_value = 1;
    bpf_setsockopt(sk, SOL_SOCKET, SO_REUSEPORT, &reuseport_value, sizeof(reuseport_value));

    if (ctx->level == SOL_IPV6 && ctx->optname == IPV6_V6ONLY) {
        // bpf_printk("disabling ipv6only");
        *optval = 0;
    }

    if (ctx->level == SOL_SOCKET && ctx->optname == SO_REUSEPORT) {
        if (*optval == 0) {
            // bpf_printk("enabling SO_REUSEPORT");
            *optval = 1;
        }
    }

    return 1;
}
