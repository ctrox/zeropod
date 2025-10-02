# Shim Grouping (Experimental)

```yaml
io.containerd.runc.v2.group: "zeropod"
```

It's possible to reduce the resource usage further by grouping multiple pods
into one shim process. The value of the annotation specifies the group id, each
of which will result in a shim process. This is currently marked as experimental
since not much testing has been done and new issues might surface when using
grouping.
