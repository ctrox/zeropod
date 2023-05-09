# zeropod - pod that scales down to 0

## TODO

- [] Support more than 1 container in a zeropod
	- [] Support scaling more than 1 container in a zeropod (use-cases might be limited here)
- [] Fix logs after restore
- [] Visibility into state (scaled down or up) from the outside
	- [] k8s events?
	- [] Metrics
	- [] Custom resource that syncs with a zeropod?
- [] Create installer DaemonSet (runtime shim, containerd config, criu binary)
- [] e2e testing
- [] Scale down/up on demand instead of time based
