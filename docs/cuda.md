# CUDA Checkpoint Support

Zeropod supports checkpointing and restoring CUDA-accelerated applications using [CRIU](https://criu.org/Main_Page) and the [cuda-checkpoint](https://github.com/nvidia/cuda-checkpoint) utility.

## Prerequisites

To usage CUDA checkpointing, the following requirements must be met on the host nodes:

1.  **NVIDIA Drivers**: Version 550 or higher is required.
2.  **`cuda-checkpoint`**: The `cuda-checkpoint` utility must be installed and available in the system PATH (or configured location).
3.  **Zeropod CRIU Image**: The Zeropod installation must use a CRIU image capable of CUDA checkpointing (e.g., built with `criu-cuda` plugin support).

## Usage

When these prerequisites are met, Zeropod automatically detects the presence of the `criu-cuda` plugin (deployed by the Zeropod installer to `/opt/zeropod/lib/criu/cuda.so`) and enables it during checkpoint operations.

No specific pod annotation is required to enable CUDA support; it is handled transparently if the plugin is available.

## Troubleshooting

### Checkpoint Failures

If checkpointing fails for a CUDA application, check the `containerd` logs or the `dump.log` in the container's work directory (`/var/lib/zeropod/...`).

Common errors include:
- `cuda-checkpoint` not found: Ensure the utility is installed on the host.
- Driver mismatch: Ensure the NVIDIA driver version is compatible.
- Plugin not found: Ensure the `cuda.so` plugin exists in `/opt/zeropod/lib/criu/`.

### Limitations

- CUDA support relies on the underlying capabilities of `cuda-checkpoint` and CRIU.
- Some advanced CUDA features or specific memory types (like UVM or IPC) might have limited support depending on the `cuda-checkpoint` version.
