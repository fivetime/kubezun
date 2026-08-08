# Deploying a tenant's virtual node

Everything here is per tenant, not per node: one process serves all of a
tenant's virtual nodes (per architecture, per availability zone), sharing their
informers and their credential.

| File | What it is |
|---|---|
| `tenant-vk.yaml` | ServiceAccount, ClusterRole and the two bindings |
| `kubezun@.service` | The process, as a systemd unit |
| `serving-cert.md` | Getting a certificate for the kubelet API |
| `kyverno-tenant-policies.yaml` | Admission policies for a tenant's namespace |
| `exclude-virtual-nodes.md` | Keeping cluster DaemonSets off virtual nodes |
| `zun/` | How Zun itself is configured, on both kinds of host |

## Order

1. Apply `tenant-vk.yaml` with `<TENANT>` substituted, and write a kubeconfig
   for its ServiceAccount to `/etc/kubezun/<TENANT>/kubeconfig`.
2. Issue a serving certificate (`serving-cert.md`) into the same directory.
3. Write the tenant's application credential to
   `/etc/kubezun/<TENANT>/openrc` as `OS_*` assignments.
4. Install the unit and start it.

```sh
install -m 0700 -d /etc/kubezun/<TENANT>
install -m 0600 /dev/null /etc/kubezun/<TENANT>/openrc
```

The directory holds a credential that can create capsules in the tenant's
OpenStack project and a key that can serve its kubelet API. `0600` in a `0700`
directory is the whole of its protection — a unit file is world-readable, so
nothing secret goes in one.

## Why a process per tenant

The tenant's application credential is what the process authenticates to
OpenStack with, and it is the tenant's whole authority there. One process
serving several tenants would hold several, and a flaw in one tenant's path
would reach the others (DESIGN §4). The provider's namespace check is the
boundary on the Kubernetes side; the credential is the boundary on the
OpenStack side, and neither is shared.

## Running in the cluster instead

The natural shape for a virtual kubelet is a Deployment, and everything above
maps onto one — the credential becomes a Secret, the certificate another, the
kubeconfig a projected token. What is not settled is the address.

The API server reaches a node's kubelet API at whatever the node reports, and a
pod's address changes every time it is rescheduled. So an in-cluster process
needs either a certificate reissued on each start, the way a real kubelet
bootstraps one, or a stable name reported as an `InternalDNS` address with the
certificate covering that name — which then depends on the control plane
resolving cluster DNS. Neither is hard; they are different enough to be worth
deciding rather than defaulting into, and until then this is the shape that has
been run and verified. See DESIGN §14.
