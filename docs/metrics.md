# Metrics

The zeropod-node pod exposes metrics on `0.0.0.0:8080/metrics` in Prometheus
format on each installed node. The metrics address can be configured with the
`-metrics-addr` flag. The following metrics are currently available:

```bash
# HELP zeropod_checkpoint_duration_seconds The duration of the last checkpoint in seconds.
# TYPE zeropod_checkpoint_duration_seconds histogram
zeropod_checkpoint_duration_seconds_bucket{container="nginx",namespace="default",pod="nginx",le="+Inf"} 3
zeropod_checkpoint_duration_seconds_sum{container="nginx",namespace="default",pod="nginx"} 0.749254206
zeropod_checkpoint_duration_seconds_count{container="nginx",namespace="default",pod="nginx"} 3
# HELP zeropod_last_checkpoint_time A unix timestamp in nanoseconds of the last checkpoint.
# TYPE zeropod_last_checkpoint_time gauge
zeropod_last_checkpoint_time{container="nginx",namespace="default",pod="nginx"} 1.688065891505882e+18
# HELP zeropod_last_restore_time A unix timestamp in nanoseconds of the last restore.
# TYPE zeropod_last_restore_time gauge
zeropod_last_restore_time{container="nginx",namespace="default",pod="nginx"} 1.688065880496497e+18
# HELP zeropod_restore_duration_seconds The duration of the last restore in seconds.
# TYPE zeropod_restore_duration_seconds histogram
zeropod_restore_duration_seconds_bucket{container="nginx",namespace="default",pod="nginx",le="+Inf"} 4
zeropod_restore_duration_seconds_sum{container="nginx",namespace="default",pod="nginx"} 0.684013211
zeropod_restore_duration_seconds_count{container="nginx",namespace="default",pod="nginx"} 4
# HELP zeropod_running Reports if the process is currently running or checkpointed.
# TYPE zeropod_running gauge
zeropod_running{container="nginx",namespace="default",pod="nginx"} 0
# HELP zeropod_checkpoint_errors_total Total number of checkpoint errors.
# TYPE zeropod_checkpoint_errors_total counter
zeropod_checkpoint_errors_total{container="nginx",namespace="default",pod="nginx"} 0
# HELP zeropod_restore_errors_total Total number of restore errors.
# TYPE zeropod_restore_errors_total counter
zeropod_restore_errors_total{container="nginx",namespace="default",pod="nginx"} 0
```
