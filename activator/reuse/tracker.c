//go:build ignore

#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

char __license[] SEC("license") = "Dual MIT/GPL";

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u32); // dest port
    __type(value, __u64); // timestamp
    __uint(max_entries, 128); // room for 128 ports in a container
} socket_tracker SEC(".maps");

SEC("cgroup_skb/ingress")
int track_ingress(struct __sk_buff *skb) {
    __u32 dport = skb->local_port;
    if (dport == 0) {
        return 1;
    }

    __u64 time = bpf_ktime_get_ns();
    bpf_map_update_elem(&socket_tracker, &dport, &time, BPF_ANY);

    return 1;
}
