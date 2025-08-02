//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"
#include "bpf_endian.h"

char __license[] SEC("license") = "Dual MIT/GPL";

#define TC_ACT_OK   0
#define ETH_P_IP    0x0800
#define ETH_P_IPV6  0x86DD
#define NEXTHDR_TCP 6

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
            // if we can find an active connection on the source port, we need
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

static __always_inline struct ipv6hdr* ipv6_header(void *data, void *data_end) {
    struct ethhdr *eth = data;
    struct ipv6hdr *ip6;

    if (data + sizeof(*eth) + sizeof(*ip6) > data_end) {
        return NULL;
    }

    if (bpf_ntohs(eth->h_proto) != ETH_P_IPV6) {
        return NULL;
    }

    ip6 = data + sizeof(*eth);
    return ip6;
}

static __always_inline struct iphdr* ipv4_header(void *data, void *data_end) {
    struct ethhdr *eth = data;
    struct iphdr *ip4;

    if (data + sizeof(*eth) + sizeof(*ip4) > data_end) {
        return NULL;
    }

    if (bpf_ntohs(eth->h_proto) != ETH_P_IP) {
        return NULL;
    }

    ip4 = data + sizeof(*eth);
    return ip4;
}

static __always_inline int parse_and_redirect(struct __sk_buff *ctx, bool ingress) {
    void *data_end = (void *)(long)ctx->data_end;
    void *data = (void *)(long)ctx->data;
    struct tcphdr *tcp = NULL;

    struct iphdr *ip4 = ipv4_header(data, data_end);
    if (ip4) {
        if ((void*)ip4 + sizeof(*ip4) <= data_end) {
            if (ip4->protocol == IPPROTO_TCP) {
                tcp = (void*)ip4 + sizeof(*ip4);
            }
        }
    } else {
        struct ipv6hdr *ip6 = ipv6_header(data, data_end);
        if (ip6) {
            if ((void*)ip6 + sizeof(*ip6) <= data_end) {
                if (ip6->nexthdr == NEXTHDR_TCP) {
                    tcp = (void*)ip6 + sizeof(*ip6);
                }
            }
        }
    }

    if ((tcp != NULL) && ((void*)tcp + sizeof(*tcp) <= data_end)) {
        if (ingress) {
            return ingress_redirect(tcp);
        }
        return egress_redirect(tcp);
    }

    return TC_ACT_OK;
}


SEC("tc")
int tc_redirect_ingress(struct __sk_buff *skb) {
    return parse_and_redirect(skb, true);
}

SEC("tc")
int tc_redirect_egress(struct __sk_buff *skb) {
    return parse_and_redirect(skb, false);
}
