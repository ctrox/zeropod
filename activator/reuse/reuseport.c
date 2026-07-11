//go:build ignore
// SPDX-License-Identifier: GPL-2.0

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include "bpf_endian.h"

char __license[] SEC("license") = "Dual MIT/GPL";

// key 0: app listener
// key 1: wake listener
// key 2: probe listener
struct {
    __uint(type, BPF_MAP_TYPE_REUSEPORT_SOCKARRAY);
    __type(key, __u32);
    __type(value, __u64);
    __uint(max_entries, 3);
} listeners SEC(".maps");

#define ETH_P_IP    0x0800
#define ETH_P_IPV6  0x86DD
#define AF_INET6    10

SEC("sk_reuseport/migrate")
int select_or_migrate(struct sk_reuseport_md *md)
{
    __u32 app = 0;
    __u32 wake = 1;
    __u32 probe = 2;

    if (!bpf_sk_select_reuseport(md, &listeners, &app, 0))
        return SK_PASS;
    if (!bpf_sk_select_reuseport(md, &listeners, &wake, 0))
        return SK_PASS;
    if (!bpf_sk_select_reuseport(md, &listeners, &probe, 0))
        return SK_PASS;

    return SK_DROP;
}
