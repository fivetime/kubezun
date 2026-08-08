# kubezun

A virtual-kubelet provider that runs a tenant's Kubernetes pods as OpenStack Zun
capsules, so the tenant gets a cluster — nodes, DaemonSets, Services, `kubectl
logs`, `kubectl exec` — without owning a single worker machine.

The tenant sees a node. Behind it there is no machine: a process registers the
node, accepts the pods scheduled onto it, and creates a capsule per pod on the
platform's OpenStack. Each capsule is a Kata VM with its own port on the
tenant's Neutron network, so a pod's address is a tenant network address and
isolation is the hypervisor's rather than the kernel's.

## Where this came from

This repository began as the archived
[virtual-kubelet/openstack-zun](https://github.com/virtual-kubelet/openstack-zun)
provider — 745 lines against a node-cli that no longer exists, last touched in
2021. Only its status mapping survives. Everything else was rewritten; see
`docs/DESIGN.md` §11 for what was kept and why.

It depends on a **fork** of Zun, not on upstream Zun:
[fivetime/openstack-zun](https://github.com/fivetime/openstack-zun). Upstream
Zun has no `logs` or `exec` endpoint for a capsule, will not accept a
capsule-only compute host, executes no probes, and rejects the `nets` key that
puts a capsule on a chosen network. Those are the fork's commits. A stock Zun
will not serve this provider.

## Reading order

| Start here | For |
|---|---|
| `docs/DESIGN.md` | Why it is shaped this way. §13 lists what has been rejected and should not be proposed again |
| `docs/bootstrap.md` | Building the platform: compute hosts, the Zun fork, a tenant's OpenStack, policy |
| `deploy/README.md` | Bringing up one tenant's virtual node |
| `docs/tenant-guide.md` | What a tenant can and cannot do |
| `TODO.md` | What is done and what is not |

`docs/bootstrap.md` and `deploy/` are the two halves of an installation: the
first is the platform, once; the second is per tenant, repeatedly.

## What it does and does not do

Runs pods as capsules, with volumes from ConfigMaps and Secrets, `logs` and
`exec` through the kubelet API, liveness probes that restart a container while
keeping its address, and readiness delegated to the load balancer's health
monitor. Services become Octavia load balancers on the tenant's network. Nodes
carry the well-known labels, report a capacity mirrored from the tenant's quota,
and can be one per architecture so a mixed-architecture tenant schedules
correctly.

It does not enforce NetworkPolicy — an important gap, since a tenant's several
namespaces all land in one OpenStack project on one network, and a policy a
tenant writes is accepted and does nothing. It has no Ingress yet. Probes on
distroless images fail, because the probe is rewritten into something executed
inside the container and such an image has nothing to execute.

## Its place

kubezun is one tier of a larger platform. [KubeZoo](https://github.com/kubewharf/kubezoo)
gives each tenant its view of the cluster; kubezun gives that view somewhere to
run. A tenant on the other tier runs pods on the platform's own Kata pool
instead, through the same view. `docs/DESIGN.md` §1 has the matrix.
