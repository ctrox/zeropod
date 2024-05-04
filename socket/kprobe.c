//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"

char __license[] SEC("license") = "Dual MIT/GPL";

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024); // should be enough pids?
	__type(key, __u32);   // pid
	__type(value, __u64); // ktime ns of the last tracked event
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} tcp_events SEC(".maps");

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

	__u64 time = bpf_ktime_get_ns();

	// const char fmt_str[] = "%d: accept found on pid %d\n";
	// bpf_trace_printk(fmt_str, sizeof(fmt_str), time, pid);

	return bpf_map_update_elem(tcp_event, &pid, &time, BPF_ANY);
};
