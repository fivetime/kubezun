# Zun, as this project needs it configured

The fork's code lives in its own repository. What is here is the configuration,
which lived nowhere: it existed only on the lab machines, so rebuilding them
would have lost it, and every setting below was arrived at by hitting the
failure it prevents.

| File | Goes on | What it is |
|---|---|---|
| `zun.conf.compute` | every capsule host | zun-compute and zun-cni-daemon |
| `zun.conf.controller` | the API host | zun-api and zun-conductor |
| `10-zun-cni.conf` | every capsule host | `/etc/cni/zun-net.d/`, verbatim |

Secrets are `<PLACEHOLDER>`. Nothing here is a working value, and no credential
belongs in this repository.

## The settings worth knowing about

**`container_driver = cri` on compute.** A capsule host runs no Docker daemon.
Upstream Zun refuses to start without one because `load_container_driver` did
not consider a capsule-only host; the fork's first commits are what make this
value legal.

**`default_cpu = 0`, `default_memory = 0`.** Upstream defaults are 1.0 CPU and
512 MB, applied to any container that does not ask. A BestEffort pod asks for
nothing, so it would be silently capped at 512 MB and then OOM-killed — with
nothing on the Kubernetes side to explain it, since the pod's own spec has no
limit to point at. This is the one setting here whose absence produces a bug
that looks like the application's fault.

**`[os_vif_ovs] ovsdb_connection`.** Open vSwitch runs in a container on these
nodes, so its socket is the only way to reach the database; the default
`tcp:127.0.0.1:6640` has no listener. It fails as `binding_failed` on the port
with no mention of ovsdb, which reads as a Neutron problem and is not one.

**`container_runtime = kata-qemu`.** The other two handlers configured on these
nodes are `kata-clh` and `kata-fc`. This is per host, not per capsule.

## Checking what a host is actually running

The lab's `/opt/stack/zun` is a working tree with uncommitted modifications, so
`git log` there answers a different question than the one being asked. Compare
file hashes against the fork's master instead.

Check a **compute** node. The API host also has the CRI driver's source and
never executes it, so its copy can be stale without anything being wrong — and
reading it as though it were live has already produced one confident wrong
report about which code was deployed.
