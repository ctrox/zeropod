
//go:build ignore

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>

char __license[] SEC("license") = "Dual MIT/GPL";

#define NEXTHDR_TCP 6

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u32); // dest port
    __type(value, __u64); // timestamp
    __uint(max_entries, 128); // room for 128 ports in a container
} socket_tracker SEC(".maps");

struct ip_key {
    __u32 prefixlen;
    __u8 addr[16];
};

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __type(key, struct ip_key);
    __type(value, __u8);
    __uint(max_entries, 16);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} ignored_addrs SEC(".maps");

SEC("tc")
int track_ingress(struct __sk_buff *skb) {
    __u16 proto = skb->protocol;

    if (proto == __bpf_constant_htons(ETH_P_IP)) {
        struct iphdr ip;
        if (bpf_skb_load_bytes_relative(skb, 0, &ip, sizeof(ip), BPF_HDR_START_NET) < 0) {
            return 0;
        }

        if (ip.protocol == NEXTHDR_TCP) {
            struct ip_key key = {};
            key.prefixlen = 32;
            __builtin_memcpy(&key.addr, &ip.saddr, 4);

            if (bpf_map_lookup_elem(&ignored_addrs, &key)) {
                return 0;
            }

            __u16 dport;
            __u32 l4_offset = ip.ihl * 4;
            if (bpf_skb_load_bytes_relative(skb, l4_offset + 2, &dport, 2, BPF_HDR_START_NET) < 0) {
                return 0;
            }

            __u32 port_key = __bpf_ntohs(dport);
            __u64 ts = bpf_ktime_get_ns();
            bpf_map_update_elem(&socket_tracker, &port_key, &ts, BPF_EXIST);
        }
    }
    else if (proto == __bpf_constant_htons(ETH_P_IPV6)) {
        struct ipv6hdr ipv6;
        if (bpf_skb_load_bytes_relative(skb, 0, &ipv6, sizeof(ipv6), BPF_HDR_START_NET) < 0) {
            return 0;
        }

        if (ipv6.nexthdr == NEXTHDR_TCP) {
            struct ip_key key = {};
            key.prefixlen = 128;
            __builtin_memcpy(&key.addr, &ipv6.saddr, 16);

            if (bpf_map_lookup_elem(&ignored_addrs, &key)) {
                return 0;
            }

            __u16 dport;
            if (bpf_skb_load_bytes_relative(skb, 40 + 2, &dport, 2, BPF_HDR_START_NET) < 0) {
                return 0;
            }

            __u32 port_key = __bpf_ntohs(dport);
            __u64 ts = bpf_ktime_get_ns();
            bpf_map_update_elem(&socket_tracker, &port_key, &ts, BPF_EXIST);
        }
    }

    return 0;
}
