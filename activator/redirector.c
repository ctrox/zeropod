//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"
#include "bpf_endian.h"
#include "bpf_core_read.h"

char __license[] SEC("license") = "Dual MIT/GPL";

#define TC_ACT_OK   0
#define ETH_P_IP    0x0800
#define ETH_P_IPV6  0x86DD
#define NEXTHDR_TCP 6

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128); // allows for 128 different ports in a single pod
    __type(key, __be16);   // sport
    __type(value, __be16); // dport
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} ingress_redirects SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128); // allows for 128 different ports in a single pod
    __type(key, __be16);   // sport
    __type(value, __be16); // dport
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} egress_redirects SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128); // allows for 128 different ports in a single pod
    __type(key, __be16); // proxy port
    __type(value, u8); // unused
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} disable_redirect SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 512); // 512 max connections while application is being restored
    __type(key, __be16); // remote_port
    __type(value, u8); // unused
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} active_connections SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 128); // allows for 128 different ports in a single pod
    __type(key, __be16);   // dport
    __type(value, __u64); // ktime ns of the last tracked event
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} socket_tracker SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1);
    __type(key, u8); // fixed identifier (0)
    __type(value, __be32); // kubelet addr
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} kubelet_addrs_v4 SEC(".maps");

struct ipv6_addr {
    __u8 u6_addr8[16];
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1);
    __type(key, u8); // fixed identifier (0)
    __type(value, struct ipv6_addr); // kubelet addr
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} kubelet_addrs_v6 SEC(".maps");

const volatile char probe_binary_name[TASK_COMM_LEN] = "";

static __always_inline int track_activity(__be16 dport) {
    __u64 time = bpf_ktime_get_ns();
    void *tracker = &socket_tracker;
    // bpf_printk("tracking activity to %d", dport);
    return bpf_map_update_elem(tracker, &dport, &time, BPF_EXIST);
}

static __always_inline int disabled(__be16 dport_h) {
    void *disable_redirect_map = &disable_redirect;
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
        if (disabled(dport_h)) {
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

static __always_inline __be32 lookup_kubelet_ip_v4(struct iphdr *ip) {
    u8 key = 0;
    char comm[TASK_COMM_LEN];
    void *kubelet_addrs = &kubelet_addrs_v4;
    __be32 *existing_addr = bpf_map_lookup_elem(kubelet_addrs, &key);
    if (existing_addr) {
        // bpf_printk("returning existing kubelet addr: %pI4", existing_addr);
        return *existing_addr;
    }
    // bpf_get_current_comm is not available in a tc program on arm64, so we use
    // bpf_get_current_task to get the comm.
    struct task_struct *task = (void *)bpf_get_current_task();
    BPF_CORE_READ_STR_INTO(&comm, task, comm);
    if (bpf_strncmp(comm, TASK_COMM_LEN, (char *)probe_binary_name) == 0) {
        // bpf_printk("found kubelet addr v4: %pI4", &ip->saddr);
        bpf_map_update_elem(kubelet_addrs, &key, &ip->saddr, BPF_ANY);
        return ip->saddr;
    }
    return 0;
}

static __always_inline bool ipv6_addr_equal(const struct in6_addr *a1, const struct in6_addr *a2) {
    if (a1 == NULL || a2 == NULL) {
        return false;
    }
    #pragma unroll
    for (int i = 0; i < 15; i++) {
        if (a1->in6_u.u6_addr8[i] != a2->in6_u.u6_addr8[i]) {
            return false;
        }
    }
    return true;
}

static __always_inline struct in6_addr* lookup_kubelet_ip_v6(struct ipv6hdr *ip) {
    char comm[TASK_COMM_LEN];
    u8 key = 0;
    void *kubelet_addrs = &kubelet_addrs_v6;
    struct in6_addr *existing_addr = bpf_map_lookup_elem(kubelet_addrs, &key);
    if (existing_addr) {
        // bpf_printk("returning existing kubelet v6 addr: %pI6", existing_addr);
        return existing_addr;
    }
    // bpf_get_current_comm is not available in a tc program on arm64, so we use
    // bpf_get_current_task to get the comm.
    struct task_struct *task = (void *)bpf_get_current_task();
    BPF_CORE_READ_STR_INTO(&comm, task, comm);
    if (bpf_strncmp(comm, TASK_COMM_LEN, (char *)probe_binary_name) == 0) {
        // bpf_printk("found kubelet addr v6: %pI6", &ip->saddr);
        bpf_map_update_elem(kubelet_addrs, &key, &ip->saddr, BPF_ANY);
        return &ip->saddr;
    }
    return NULL;
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
                if ((tcp != NULL) && ((void*)tcp + sizeof(*tcp) <= data_end)) {
                    if (ingress) {
                        if (ip4->saddr != lookup_kubelet_ip_v4(ip4)) {
                            track_activity(bpf_ntohs(tcp->dest));
                        }
                    }
                }
            }
        }
    } else {
        struct ipv6hdr *ip6 = ipv6_header(data, data_end);
        if (ip6) {
            if ((void*)ip6 + sizeof(*ip6) <= data_end) {
                if (ip6->nexthdr == NEXTHDR_TCP) {
                    tcp = (void*)ip6 + sizeof(*ip6);
                    if ((tcp != NULL) && ((void*)tcp + sizeof(*tcp) <= data_end)) {
                        if (ingress) {
                            if (!ipv6_addr_equal(&ip6->saddr, lookup_kubelet_ip_v6(ip6))) {
                                track_activity(bpf_ntohs(tcp->dest));
                            }
                        }
                    }
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
