# Bringing up the platform

From an OpenStack cloud and a Kubernetes cluster to a host that can run
capsules. This is the part done once; `deploy/README.md` is the part done per
tenant.

Everything below states the end state to reach and the values it was verified
at, on the lab of 2026-08-08. Where a step is somebody else's installer — Kata's
packaging, devstack, Kyverno's release — this says what the result has to look
like rather than restating their instructions, because the result is the part
that is specific to this project and the instructions are not.

## What has to exist first

An OpenStack cloud with Keystone, Neutron with ML2/OVN, Octavia, and Glance. A
Kubernetes cluster whose worker nodes are separate from where capsules run, or
the same nodes with the runtimes split as below. KubeZoo, if tenants are to have
their own view of the cluster — this project does not require it, but the
namespace prefixes in `deploy/` assume it.

Zun is **not** a prerequisite: the fork is installed here, and a stock Zun will
not serve this provider.

## 1. The compute host

A capsule host runs two container runtimes that must not be confused for one
another. containerd's CRI plugin hardcodes the `k8s.io` namespace, so a kubelet
and Zun cannot share one containerd without colliding.

**kubelet gets CRI-O.** In `/var/lib/kubelet/config.yaml`:

```yaml
containerRuntimeEndpoint: unix:///var/run/crio/crio.sock
```

**Zun gets containerd, the whole instance**, on the default socket
`/run/containerd/containerd.sock` with root `/var/lib/containerd`. Nothing else
may use it. Its CNI directory is pointed away from the cluster's:

```toml
# /etc/containerd/config.toml
imports = ['/etc/containerd/conf.d/*.toml']

[plugins.'io.containerd.cri.v1.runtime'.cni]
  conf_dir = '/etc/cni/zun-net.d'
```

Pointing it at the cluster's `/etc/cni/net.d` instead gives every capsule the
cluster CNI's addressing, which is the one thing the tenant network exists to
avoid.

### Kata

Kata Containers 3.31.0, installed as the static package under `/opt/kata`, with
`/opt/kata/bin` holding `containerd-shim-kata-v2` and the hypervisors
(`cloud-hypervisor`, `firecracker`, `jailer`). Three configurations in
`/etc/kata-containers/`: `configuration.toml` (QEMU),
`configuration-clh.toml`, `configuration-fc.toml`.

Register the handlers in a drop-in rather than in `config.toml`, so a containerd
upgrade does not take them with it:

```toml
# /etc/containerd/conf.d/kata.toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata-qemu]
  runtime_type = 'io.containerd.kata.v2'
  privileged_without_host_devices = true
  pod_annotations = ['io.katacontainers.*']
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata-qemu.options]
    ConfigPath = '/etc/kata-containers/configuration.toml'
```

and the same shape for `kata-clh` and `kata-fc` against their configurations.
`container_runtime` in `zun.conf` selects which handler this host uses.

The Firecracker handler needs a devmapper thin-pool, which does not survive a
reboot because it is built on loopback devices. A unit recreates it before
containerd starts:

```
# kata-fc-thinpool.service → /usr/local/sbin/kata-fc-thinpool.sh
dmsetup info "$POOL" >/dev/null 2>&1 && exit 0     # idempotent
DATA_DEV=$(losetup -j "$DM/data" | ...)            # reuse or attach
dmsetup create "$POOL" --table "0 $SECTORS thin-pool $META_DEV $DATA_DEV 128 32768 1 skip_block_zeroing"
```

Without it, containerd starts, finds no pool, and every kata-fc capsule fails to
create with an error naming the snapshotter rather than the pool.

### CNI

```json
// /etc/cni/zun-net.d/10-zun-cni.conf
{ "cniVersion": "0.3.1", "name": "zun", "type": "zun-cni",
  "zun_conf": "/etc/zun/zun.conf" }
```

The `zun-cni` binary and `zun-cni-daemon` come from the fork. On a CRI host this
replaces kuryr-libnetwork entirely.

## 2. Zun

Install the fork — `github.com/fivetime/openstack-zun`, branch `master`. There is
one branch on purpose; see CLAUDE.md.

On the lab it is a devstack installation, so the services are `devstack@`
units running out of a virtualenv:

| Host | Unit | Binary |
|---|---|---|
| API | `devstack@zun-api` | `/opt/stack/data/venv/bin/zun-api`, user `stack` |
| compute | `devstack@zun-compute` | `/opt/stack/venv/bin/zun-compute`, user `root` |
| compute | `devstack@zun-cni-daemon` | `/opt/stack/venv/bin/zun-cni-daemon`, user `root` |

All three read `/etc/zun/zun.conf`. `deploy/zun/` has that file for both kinds
of host, with the settings that matter annotated — read it before writing one,
particularly `default_cpu`/`default_memory` and `[os_vif_ovs] ovsdb_connection`.

Register the service in Keystone as type `container`:

```sh
openstack service create --name zun container
openstack endpoint create --region RegionOne container public   http://<API HOST>:9517/v1
openstack endpoint create --region RegionOne container internal http://<API HOST>:9517/v1
openstack endpoint create --region RegionOne container admin    http://<API HOST>:9517/v1
```

Verify by asking the API which compute hosts have registered. The `openstack`
CLI has no Zun commands unless python-zunclient's plugin is installed, and on
the lab it is not, so this goes straight at the endpoint:

```sh
curl -s -H "X-Auth-Token: $(openstack token issue -f value -c id)" \
     -H "OpenStack-API-Version: container 1.40" \
     http://<API HOST>:9517/v1/services
```

Every capsule host should appear as `zun-compute` in state `up`.

## 3. Octavia

A Service becomes a load balancer, and which driver builds it decides whether
that costs a virtual machine. The lab has three drivers enabled:

```
incus     OVN L4 frontend with ALL_ACTIVE containerized L7 workers   ← default
ovn       OVN provider driver (amphora-less L4)
amphora   Octavia amphora driver (haproxy VM)
```

kubezun asks for `ovn` explicitly at create time (`reconciler.go`, `ovnProvider`)
rather than taking `default_provider_driver`, because the default is a site
decision and a ClusterIP has no business costing an instance. The OVN driver
translates the load balancer into OVN northbound rules, so it is free and
distributed, and it carries L4 only — which is all a ClusterIP is. Ingress is
L7 and will need one of the others.

⚠️ The address port. An OVN-provider load balancer reports a `vip_port_id` that
stops resolving shortly after creation while the load balancer keeps working.
Do not treat that as a fault: acting on it once cost every tenant Service a new
address every few minutes (`reconciler.go` records it).

## 4. A tenant's OpenStack

One project per tenant, holding everything that tenant's capsules touch. On the
lab, tenant `111111` is project `knaas-t1`:

```sh
openstack project create knaas-t1
openstack network create t1-net
openstack subnet create t1-subnet --network t1-net --subnet-range 192.168.100.0/24
openstack network create t1-vip-net
openstack subnet create t1-vip-subnet --network t1-vip-net --subnet-range 192.168.200.0/24
openstack router create t1-router
openstack router set t1-router --external-gateway public
openstack router add subnet t1-router t1-subnet
openstack router add subnet t1-router t1-vip-subnet
```

Two subnets, not one: capsules take addresses from `t1-subnet` and Service load
balancers from `t1-vip-subnet`. Keeping them apart means a Service address is
never mistaken for a pod address and the router sits in front of both.

Then a credential the virtual kubelet will authenticate with:

```sh
openstack application credential create kubezun-t1 --unrestricted
```

Scoped to that project, and **never an admin credential**: Zun's database layer
treats an admin context as spanning every project, so an admin credential in one
tenant's process is every tenant's capsules. `deploy/README.md` covers where the
credential goes on disk.

The network ids from the steps above are what `--network-id`, `--vip-network-id`
and `--vip-subnet-id` name in the unit file.

## 5. Admission policy

Kyverno, with the policies in `deploy/kyverno-tenant-policies.yaml`:

```
knaas-tenant-placement             pods onto the tenant's virtual node
knaas-tenant-daemonset-placement   the same, on DaemonSet.spec.template
knaas-tenant-pod-capabilities      what a capsule pod may ask for
```

The DaemonSet one is separate because mutating a DaemonSet's pods is too late —
the controller decides which nodes are eligible from the template, so the
template is what has to carry the placement.

All three select on namespaces carrying `knaas.io/tenant`, so a namespace
without that label gets no placement and its pods are scheduled by the ordinary
scheduler onto ordinary nodes — silently, since nothing reports a policy that
did not match. KubeZoo's own `kubezoo.io/tenant` is a different label with a
different meaning: it marks every namespace belonging to a tenant, including the
ones KubeZoo manages itself. A tenant's `<id>-kube-system` carries that one and
not `knaas.io/tenant`, which is why anything KubeZoo runs there — its CoreDNS,
for one — stays on cluster nodes rather than moving to the virtual node.

`deploy/exclude-virtual-nodes.md`
covers the other direction: keeping the cluster's own DaemonSets off virtual
nodes, which is not optional — a node with no machine behind it cannot run a log
shipper or a CNI agent, and they will try forever.

## Then

`deploy/README.md`, once per tenant.

## Known to be missing

NetworkPolicy is not enforced for capsules and a tenant's policies silently do
nothing. Serving certificates for the kubelet API have no issuing mechanism here
(`deploy/serving-cert.md` covers the shape, not the issuer). Probes fail on
distroless images. `TODO.md` is the current list.
