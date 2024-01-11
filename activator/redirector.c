//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"
#include "bpf_endian.h"

char __license[] SEC("license") = "Dual MIT/GPL";

#define TC_ACT_OK 0

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128);
    __type(key, __be16);   // sport
    __type(value, __be16); // dport
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} ingress_redirects SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128);
    __type(key, __be16);   // sport
    __type(value, __be16); // dport
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} egress_redirects SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 512);
    __type(key, __be16); // proxy port
    __type(value, u8); // unused
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} disable_redirect SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 512); // TBD but should probably be enough
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

static __always_inline int ingress_redirect(struct tcphdr *tcp) {
    __be16 sport_h = bpf_ntohs(tcp->source);
    __be16 dport_h = bpf_ntohs(tcp->dest);

    void *active_connections_map = &active_connections;

    void *redirect_map = &ingress_redirects;
    __be16 *new_dest = bpf_map_lookup_elem(redirect_map, &dport_h);

    if (new_dest) {
        // check ports which should not be redirected
        if (disabled(sport_h, dport_h)) {
            // if we can find an acive connection on the source port, we need
            // to redirect regardless until the connection is closed.
            void *conn_sport = bpf_map_lookup_elem(active_connections_map, &sport_h);
            if (!conn_sport) {
                // bpf_printk("ingress: sport %d or dport %d is disabled for redirecting", sport_h, dport_h);
                return TC_ACT_OK;
            }
            // bpf_printk("ingress: port %d found in active connections, redirecting", sport_h);
        }
        // bpf_printk("ingress: changing destination port from %d to %d for packet from %d", dport_h, *new_dest, sport_h);
        tcp->dest = bpf_htons(*new_dest);
    }

    return TC_ACT_OK;
}

static __always_inline int egress_redirect(struct tcphdr *tcp) {
    __be16 sport_h = bpf_ntohs(tcp->source);
    // __be16 dport_h = bpf_ntohs(tcp->dest);

    void *redirect_map = &egress_redirects;
    __be16 *new_source = bpf_map_lookup_elem(redirect_map, &sport_h);

    if (new_source) {
        // bpf_printk("egress: changing source port from %d to %d for packet to %d", sport_h, *new_source, dport_h);
        tcp->source = bpf_htons(*new_source);
    }

    return TC_ACT_OK;
}

static __always_inline int parse_and_redirect(struct __sk_buff *ctx, bool ingress) {
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;
    struct ethhdr *eth = data;

    if ((void*)eth + sizeof(*eth) <= data_end) {
        struct iphdr *ip = data + sizeof(*eth);

        if ((void*)ip + sizeof(*ip) <= data_end) {
            if (ip->protocol == IPPROTO_TCP) {
                struct tcphdr *tcp = (void*)ip + sizeof(*ip);
                if ((void*)tcp + sizeof(*tcp) <= data_end) {
                    if (ingress) {
                        return ingress_redirect(tcp);
                    }

                    return egress_redirect(tcp);
                }
            }
        }
    }

    return 0;
}


SEC("tc")
int tc_redirect_ingress(struct __sk_buff *skb) {
    return parse_and_redirect(skb, true);
}

SEC("tc")
int tc_redirect_egress(struct __sk_buff *skb) {
    return parse_and_redirect(skb, false);
}
