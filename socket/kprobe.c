//go:build ignore

#include "vmlinux.h"
#include "ptregs.h"
#include "bpf_helpers.h"
#include "bpf_tracing.h"
#include "bpf_core_read.h"
#include "bpf_endian.h"

char __license[] SEC("license") = "Dual MIT/GPL";

#define AF_INET 2

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1024); // should be enough pids?
    __type(key, __u32);   // pid
    __type(value, __u64); // ktime ns of the last tracked event
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} tcp_events SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1024);
    __type(key, __be32); // pod addr
    __type(value, __be32); // kubelet addr
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} pod_kubelet_addrs_v4 SEC(".maps");

struct ipv6_addr {
    __u8 u6_addr8[16];
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1024);
    __type(key, struct ipv6_addr); // pod addr
    __type(value, struct ipv6_addr); // kubelet addr
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} pod_kubelet_addrs_v6 SEC(".maps");

const volatile char probe_binary_name[TASK_COMM_LEN] = "";

static __always_inline bool ipv6_addr_equal(const struct ipv6_addr *a1, const struct ipv6_addr *a2) {
    #pragma unroll
    for (int i = 0; i < 15; i++) {
        if (a1->u6_addr8[i] != a2->u6_addr8[i]) {
            return false;
        }
    }
    return true;
}

SEC("kretprobe/inet_csk_accept")
int kretprobe__inet_csk_accept(struct pt_regs *ctx) {
    struct task_struct* task = (struct task_struct*)bpf_get_current_task_btf();
    // we use the tgid as our pid as it represents the pid from userspace
    __u32 pid = task->tgid;

    void *tcp_event = &tcp_events;
    void *found_pid = bpf_map_lookup_elem(tcp_event, &pid);

    if (!found_pid) {
        // try ppid, our process might have forks
        pid = task->real_parent->tgid;

        void *found_ppid = bpf_map_lookup_elem(tcp_event, &pid);
        if (!found_ppid) {
            return 0;
        }
    }

    struct sock *sk = (struct sock *)PT_REGS_RC(ctx);
    if (sk == NULL) {
        return 0;
    }

    if (BPF_CORE_READ(sk, __sk_common.skc_family) == AF_INET) {
        __be32 pod_addr = 0;
        __be32 daddr = 0;
        BPF_CORE_READ_INTO(&pod_addr, sk, __sk_common.skc_rcv_saddr);
        BPF_CORE_READ_INTO(&daddr, sk, __sk_common.skc_daddr);
        void *addrs = &pod_kubelet_addrs_v4;
        __be32 *kubelet_addr = bpf_map_lookup_elem(addrs, &pod_addr);
        if (kubelet_addr && *kubelet_addr == daddr) {
            return 0;
        }
    } else {
        struct ipv6_addr pod_addr;
        struct ipv6_addr daddr;
        BPF_CORE_READ_INTO(&pod_addr, sk, __sk_common.skc_v6_rcv_saddr.in6_u);
        BPF_CORE_READ_INTO(&daddr, sk, __sk_common.skc_v6_daddr.in6_u);
        void *addrs = &pod_kubelet_addrs_v6;
        struct ipv6_addr *kubelet_addr = bpf_map_lookup_elem(addrs, &pod_addr);
        if (kubelet_addr && ipv6_addr_equal(kubelet_addr, &daddr)) {
            return 0;
        }
    }

    __u64 time = bpf_ktime_get_ns();
    return bpf_map_update_elem(tcp_event, &pid, &time, BPF_ANY);
};

static int find_potential_kubelet_ip(struct sock *sk) {
    char comm[TASK_COMM_LEN];
    bpf_get_current_comm(&comm, sizeof(comm));
    if (bpf_strncmp(comm, TASK_COMM_LEN, (char *)probe_binary_name) == 0) {
        if (BPF_CORE_READ(sk, __sk_common.skc_family) == AF_INET) {
            __be32 saddr = 0;
            __be32 daddr = 0;
            BPF_CORE_READ_INTO(&saddr, sk, __sk_common.skc_rcv_saddr);
            BPF_CORE_READ_INTO(&daddr, sk, __sk_common.skc_daddr);
            void *addrs = &pod_kubelet_addrs_v4;
            __be32 *kubelet_addr = bpf_map_lookup_elem(addrs, &daddr);
            if (kubelet_addr) {
                bpf_map_update_elem(addrs, &daddr, &saddr, 0);
            }
        } else {
            struct ipv6_addr saddr;
            struct ipv6_addr daddr;
            BPF_CORE_READ_INTO(&saddr, sk, __sk_common.skc_v6_rcv_saddr.in6_u);
            BPF_CORE_READ_INTO(&daddr, sk, __sk_common.skc_v6_daddr.in6_u);
            void *addrs = &pod_kubelet_addrs_v6;
            struct ipv6_addr *kubelet_addr = bpf_map_lookup_elem(addrs, &daddr);
            if (kubelet_addr) {
                bpf_map_update_elem(addrs, &daddr, &saddr, 0);
            }
        }
    }
    return 0;
}

SEC("kprobe/tcp_rcv_state_process")
int BPF_KPROBE(tcp_rcv_state_process, struct sock *sk) {
    if (BPF_CORE_READ(sk, __sk_common.skc_state) != TCP_SYN_SENT) {
        return 0;
    }

    return find_potential_kubelet_ip(sk);
}
