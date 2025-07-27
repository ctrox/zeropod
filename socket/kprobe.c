//go:build ignore

#include "vmlinux.h"
#include "ptregs.h"
#include "bpf_helpers.h"
#include "bpf_tracing.h"
#include "bpf_core_read.h"
#include "bpf_endian.h"


char __license[] SEC("license") = "Dual MIT/GPL";

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 1024); // should be enough pids?
	__type(key, __u32);   // pid
	__type(value, __u64); // ktime ns of the last tracked event
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} tcp_events SEC(".maps");

#define AF_INET    2
#define AF_INET6   10

const volatile __u64 targ_min_us = 0;
const volatile pid_t targ_tgid = 0;

struct piddata {
  char comm[TASK_COMM_LEN];
  u64 ts;
  u32 tgid;
};

struct {
  __uint(type, BPF_MAP_TYPE_LRU_HASH);
  __uint(max_entries, 1024);
  __type(key, __be32); // pod addr
  __type(value, __be32); // kubelet addr
  __uint(pinning, LIBBPF_PIN_BY_NAME);
} pod_kubelet_addrs SEC(".maps");

SEC("kretprobe/inet_csk_accept")
int kretprobe__inet_csk_accept(struct pt_regs *ctx)
{

	// TODO: we don't check if the protocol is actually TCP here as this seems quite messy:
	// https://github.com/iovisor/bcc/blob/71b5141659aaaf4a7c2172c73a802bd86a256ecd/tools/tcpaccept.py#L118
	// does this matter? Which other protocols make use of inet_csk_accept?

	struct task_struct* task = (struct task_struct*)bpf_get_current_task_btf();
	// we use the tgid as our pid as it represents the pid from userspace
	__u32 pid = task->tgid;

	void *tcp_event = &tcp_events;
	void* found_pid = bpf_map_lookup_elem(tcp_event, &pid);

	if (!found_pid) {
		// try ppid, our process might have forks
		pid = task->real_parent->tgid;

		void* found_ppid = bpf_map_lookup_elem(tcp_event, &pid);
		if (!found_ppid) {
			return 0;
		}
	}

	struct sock *newsk = (struct sock *)PT_REGS_RC(ctx);
    if (newsk == NULL) {
        return 0;
    }

    __be32 pod_addr = 0;
    __be32 daddr = 0;
    bpf_probe_read(&pod_addr, sizeof(pod_addr), &newsk->__sk_common.skc_rcv_saddr);
    bpf_probe_read(&daddr, sizeof(daddr), &newsk->__sk_common.skc_daddr);

   	void *addrs = &pod_kubelet_addrs;
   	__be32 *kubelet_addr = bpf_map_lookup_elem(addrs, &pod_addr);
    if (kubelet_addr) {
        // bpf_printk("ignoring kubelet probe, pod: %s, kubelet: %s", pod_addr, *kubelet_addr);
        return 0;
	}

	__u64 time = bpf_ktime_get_ns();
	return bpf_map_update_elem(tcp_event, &pid, &time, BPF_ANY);
};

static int find_potential_kubelet_ip(struct sock *sk) {
    __be32 saddr = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
    __be32 daddr = BPF_CORE_READ(sk, __sk_common.skc_daddr);
    char comm[TASK_COMM_LEN];
    bpf_get_current_comm(&comm, sizeof(comm));
    if ((bpf_strncmp(comm, TASK_COMM_LEN, "kubelet") == 0) || (bpf_strncmp(comm, TASK_COMM_LEN, "k3s") == 0)) {
       	void *addrs = &pod_kubelet_addrs;
       	__be32 *kubelet_addr = bpf_map_lookup_elem(addrs, &daddr);
        if (kubelet_addr) {
            bpf_map_update_elem(addrs, &daddr, &saddr, 0);
        }
    }
    return 0;
}

SEC("kprobe/tcp_rcv_state_process")
int BPF_KPROBE(tcp_rcv_state_process, struct sock *sk)
{
    if (BPF_CORE_READ(sk, __sk_common.skc_state) != TCP_SYN_SENT) {
        return 0;
    }

    return find_potential_kubelet_ip(sk);
}
