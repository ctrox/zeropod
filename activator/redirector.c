//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"
#include "bpf_endian.h"

char __license[] SEC("license") = "Dual MIT/GPL";

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, __be16);   // sport
    __type(value, __be16); // dport
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} redirects SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 128);
    __type(key, __be16); // proxy port
    __type(value, u8); // unused
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} disable_redirect SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128); // TBD but should probably be enough
    __type(key, __be16); // remote_port
    __type(value, u8); // unused
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} active_connections SEC(".maps");

static __always_inline int disabled(__be16 sport_h, __be16 dport_h) {
    void *disable_redirect_map = &disable_redirect;

    void *disabled_s = bpf_map_lookup_elem(disable_redirect_map, &sport_h);

    if (disabled_s) {
        return 1;
    }

    void *disabled_d = bpf_map_lookup_elem(disable_redirect_map, &dport_h);

    if (disabled_d) {
        return 1;
    }

    return 0;
};

static __always_inline int redirect(struct tcphdr *tcp) {
    __be16 sport_h = bpf_ntohs(tcp->source);
    __be16 dport_h = bpf_ntohs(tcp->dest);

    void *active_connections_map = &active_connections;

    void *redirect_map = &redirects;
    __be16 *found_sport = bpf_map_lookup_elem(redirect_map, &sport_h);
    __be16 *found_dport = bpf_map_lookup_elem(redirect_map, &dport_h);

    if (found_sport) {
        if (disabled(sport_h, dport_h)) {
            void *conn_sport = bpf_map_lookup_elem(active_connections_map, &dport_h);
            if (!conn_sport) {
                // bpf_printk("port %d not in active connections, ignoring", dport_h);
                return 1;
            }
            // bpf_printk("port %d in active connections, redirecting", dport_h);
        }
        // bpf_printk("changing source port from %d to %d for packet to %d", sport_h, *found_sport, dport_h);
        tcp->source = bpf_htons(*found_sport);
    }

    if (found_dport) {
        if (disabled(sport_h, dport_h)) {
            void *conn_dport = bpf_map_lookup_elem(active_connections_map, &sport_h);
            if (!conn_dport) {
                // bpf_printk("port %d not in active connections, ignoring", sport_h);
                return 1;
            }
            // bpf_printk("port %d in active connections, redirecting", sport_h);
        }
        // bpf_printk("changing dest port from %d to %d for packet from %d", dport_h, *found_dport, sport_h);
        tcp->dest = bpf_htons(*found_dport);
    }

    return 1;
}

static __always_inline int parse_and_redirect(struct __sk_buff *ctx) {
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;
    struct ethhdr *eth = data;

    if ((void*)eth + sizeof(*eth) <= data_end) {
        struct iphdr *ip = data + sizeof(*eth);

        if ((void*)ip + sizeof(*ip) <= data_end) {
            if (ip->protocol == IPPROTO_TCP) {
                struct tcphdr *tcp = (void*)ip + sizeof(*ip);
                if ((void*)tcp + sizeof(*tcp) <= data_end) {
                    return redirect(tcp);
                }
            }
        }
    }

    return 0;
}

#define TC_ACT_OK 0

SEC("tc")
int tc_redirector(struct __sk_buff *skb) {
    parse_and_redirect(skb);
    return TC_ACT_OK;
}
