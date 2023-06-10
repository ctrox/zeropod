//go:build ignore

#include "vmlinux.h"
#include "bpf_helpers.h"

char __license[] SEC("license") = "Dual MIT/GPL";

#define IPPROTO_TCP 6
#define BPF_TCP_LISTEN 10

struct trace_event_raw_inet_sock_set_state__stub {
	__u64 unused;
	void *skaddr;
	int oldstate;
	int newstate;
	__u16 sport;
	__u16 dport;
	__u16 family;
#if __KERNEL >= 506
	__u16 protocol;
#else
	__u8 protocol;
#endif
	__u8 saddr[4];
	__u8 daddr[4];
	__u8 saddr_v6[16];
	__u8 daddr_v6[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024); // should be enough pids?
	__type(key, __u32);   // pid
	__type(value, __u64); // ktime ns of the last tracked event
} tcp_events SEC(".maps");

SEC("tracepoint/sock/inet_sock_set_state")
int inet_sock_set_state(void *ctx)
{
	struct trace_event_raw_inet_sock_set_state__stub event = {};
	if (bpf_probe_read(&event, sizeof(event), ctx) < 0) {
		return 0;
	}

	if (event.protocol != IPPROTO_TCP) {
		return 0;
	}

	if (event.newstate == BPF_TCP_LISTEN || event.oldstate == BPF_TCP_LISTEN) {
		// we don't care about listen events
		return 0;
	}

	__u32 pid = bpf_get_current_pid_tgid() >> 32;

	void *tcp_event = &tcp_events;
	void* found_pid = bpf_map_lookup_elem(tcp_event, &pid);

	if (!found_pid) {
		// try ppid, our process might have forks
		struct task_struct* task = (struct task_struct*)bpf_get_current_task_btf();
		pid = task->real_parent->tgid;

		void* found_ppid = bpf_map_lookup_elem(tcp_event, &pid);
		if (!found_ppid) {
			return 0;
		}
	}

	__u64 time = bpf_ktime_get_ns();

	// const char fmt_str[] = "tcp event pid %d: %d => %d\n";
	// bpf_trace_printk(fmt_str, sizeof(fmt_str), pid, event.sport, event.dport);

	return bpf_map_update_elem(tcp_event, &pid, &time, BPF_ANY);
}