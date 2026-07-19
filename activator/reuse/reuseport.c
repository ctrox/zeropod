//go:build ignore
// SPDX-License-Identifier: GPL-2.0

#include <linux/bpf.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/if_ether.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <stdbool.h>

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
#define AF_INET     2

volatile __u8 probe_addr[16];

SEC("sk_reuseport/migrate")
int select_or_migrate(struct sk_reuseport_md *md)
{
    __u32 app = 0;
    __u32 wake = 1;
    __u32 probe = 2;

    // if app listener is active, pass traffic directly
    if (!bpf_sk_select_reuseport(md, &listeners, &app, 0)) {
        return SK_PASS;
    }

    // when app is down, we want to select between probe and wake traffic
    // comparing the saddr to the probe_addr
    bool is_probe_traffic = false;

    struct bpf_sock *migrating = md->migrating_sk;
    if (migrating) {
        // during migration dst_ip4/dst_ip6 is the remote address
        if (migrating->family == AF_INET) {
            __u32 saddr = migrating->dst_ip4;
            if (saddr == *(volatile __u32 *)probe_addr) {
                // bpf_printk("being migrated: %pI4", &saddr);
                is_probe_traffic = true;
            }
        } else {
            if (__builtin_memcmp(migrating->dst_ip6, (void *)probe_addr, 16) == 0) {
                // bpf_printk("being migrated: %pI6", &probe_addr);
                is_probe_traffic = true;
            }
        }
    } else if (md->eth_protocol == bpf_htons(ETH_P_IP)) {
        __u32 saddr = 0;
        if (bpf_skb_load_bytes_relative(md, offsetof(struct iphdr, saddr), &saddr, sizeof(saddr), BPF_HDR_START_NET) == 0) {
            if (saddr == *(volatile __u32 *)probe_addr) {
                is_probe_traffic = true;
            }
        }
    } else if (md->eth_protocol == bpf_htons(ETH_P_IPV6)) {
        __u32 saddr6[4] = {0};
        if (bpf_skb_load_bytes_relative(md, offsetof(struct ipv6hdr, saddr), saddr6, sizeof(saddr6), BPF_HDR_START_NET) == 0) {
            if (__builtin_memcmp(saddr6, (void *)probe_addr, 16) == 0) {
                is_probe_traffic = true;
            }
        }
    }

    if (is_probe_traffic && !bpf_sk_select_reuseport(md, &listeners, &probe, 0)) {
        return SK_PASS;
    }

    if (!bpf_sk_select_reuseport(md, &listeners, &wake, 0)) {
        return SK_PASS;
    }
    return SK_DROP;
}
