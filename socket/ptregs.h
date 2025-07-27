#if defined(__TARGET_ARCH_arm64)
struct user_pt_regs {
	__u64		regs[31];
	__u64		sp;
	__u64		pc;
	__u64		pstate;
};
#endif
