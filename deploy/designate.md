# Tenant DNS through Designate

> ⚠️ **Not how this works any more, and kept for the four failure modes below
> rather than for the approach.** A tenant resolves names through its own
> CoreDNS, which reads Services through the gateway and answers with the address
> in `kubezoo.io/cluster-ip`. Nothing is published to Designate or to Neutron.
>
> The reason is not effort. What this publishes is
> `<svc>.<tid>-<namespace>.svc.cluster.local`, the platform's name for the
> Service. A tenant's application asks for `<svc>.<namespace>.svc.cluster.local`,
> because that is the namespace it can see, and **no global DNS namespace can
> serve that name at all** — every tenant needs the same name to resolve to a
> different address. A per-tenant resolver is not an optimisation here, it is
> the only arrangement that can work.
>
> Still worth reading if authoritative DNS is ever needed for something else,
> such as publishing a tenant's service under a public domain: the four silent
> failures recorded below cost a day to find and none of them announce
> themselves.

A Service's usable address is its load balancer's, not the ClusterIP the API
server assigned — a capsule has one interface, on the tenant's network, and the
cluster's service range is not on it. Applications use names, so name
resolution is not an accompaniment to the Service work, it is the other half of
it.

Neutron publishes a port's address to Designate when the port carries a DNS
name, and **removes the record when the port is deleted**. That is the reason
to route DNS through Neutron rather than writing records from kubezun: this
session found leaked floating IPs, load balancers and capsules, all of them
lifecycle bugs. A record whose life is the port's life cannot leak.

kubezun's whole part is setting `dns_name` and `dns_domain` on ports it already
knows: a Service's load balancer address port, and a pod's capsule port.

Verified end to end on the lab control node, 2026-08-07: a port created with a
name produced `rsvc.111111-default.svc.cluster.local. A 192.168.100.47` in
Designate, and deleting the port removed it.

## What has to be true

Four things, each of which fails silently or misleadingly when missing.

**1. `[DEFAULT] dns_domain` must not be the default.**

```ini
[DEFAULT]
external_dns_driver = designate
dns_domain = svc.cluster.local.
```

Left at its default of `openstacklocal`, the extension returns before doing
anything (`dns_integration.py:286-290`, reached from `:93`). The API accepts
`dns_name` and `dns_domain` on a port and stores neither, with no error
anywhere — the symptom is a port whose DNS fields come back empty.

**2. Exactly one DNS extension driver.**

```ini
[ml2]
extension_drivers = port_security,qos,subnet_dns_publish_fixed_ip
```

`subnet_dns_publish_fixed_ip` **subclasses** `dns_domain_ports`
(`subnet_dns_publish_fixed_ip.py:25-26`), which subclasses the base DNS driver.
Listing two of them runs the same logic twice and the port create fails with
`NeutronDbObjectDuplicateEntry ... PortDNS`. The subclass is the one to name:
it carries the other two.

**3. The `[designate]` section must be the one Neutron reads.**

The stock `neutron.conf` already contains a commented-out `[designate]`
section. oslo.config takes the first, so settings appended at the end of the
file are ignored — `url` stays empty, the client falls back to catalog lookup,
and the failure reads `EndpointNotFound: Could not find requested endpoint in
Service Catalog`, which points at the catalog rather than at the config.

```ini
[designate]
url = http://<control>:9001/v2
auth_type = password
auth_url = http://<control>/identity
username = designate
password = <password>
project_name = service
project_domain_name = Default
user_domain_name = Default
allow_reverse_dns_lookup = False
valid_interfaces = public
```

**4. The subnet must publish fixed IPs.**

```sh
openstack subnet set --dns-publish-fixed-ip <tenant subnet>
```

Without it only floating IPs are published, and a capsule's address is a fixed
IP. The attribute needs driver 2 loaded to be storable at all — before that it
silently reads back as unset.

Designate itself also needs `interface = public` in `[keystone_authtoken]` on a
deployment that publishes only a public identity endpoint, or every request
answers 500 with `internal endpoint for identity service not found`.

## Names

`<dns_name>.<dns_domain>`. A tenant has one network and may have several
namespaces, so the domain is set **per port** rather than on the network —
`111111-default.svc.cluster.local.` with `dns_name: rsvc` gives
`rsvc.111111-default.svc.cluster.local.`, which is the name a chart already
expects. The per-port domain is what driver 2 adds.

One Designate zone per namespace, created when the namespace is.

## Still to decide

**What capsules resolve against.** Their `resolv.conf` comes from the subnet's
`dns_nameservers`, which the Zun driver reads and passes into the sandbox
(measured: `8.8.8.8`). Pointing that straight at Designate's backend breaks
every public name — that backend is authoritative only and does not recurse.
What capsules need is a recursive resolver that holds these zones as
authoritative and recurses for everything else.

**Whether tenants may resolve each other's names.** One resolver holding every
zone lets a tenant look up another's service names and addresses. The addresses
are unreachable across tenants, so it is disclosure rather than a path, but it
is a decision rather than an oversight. Per-tenant views close it.
