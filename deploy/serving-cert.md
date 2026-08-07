# The virtual node's serving certificate

The API server reaches a node's kubelet API at the address in
`node.Status.Addresses` and the port in
`node.Status.DaemonEndpoints.kubeletEndpoint` (`kubelet_client.go:188-215`).
Node names never appear in TLS, and ports are not part of a certificate's
identity.

Two things follow, and they set the whole cost model:

- **One certificate per process, not per node.** All of a tenant's virtual nodes
  run in one process behind one address, so one certificate with that address in
  its SANs serves every one of them however many ports they listen on.
- **The per-node part is the authorizer, not the key.** Each node's endpoint
  asks the API server "may this caller `get nodes/<this node>/log`", so each node
  needs its own listen address to keep that question exact. Ports are cheap:
  a host runs out of memory for virtual-kubelet processes long before it runs
  out of ports.

## Issuing one

The cluster's own `kubernetes.io/kubelet-serving` signer works, which means the
API server already trusts the result and nothing has to be configured on the
control plane:

```sh
TENANT=111111
NODE=111111-node-az1        # any of the tenant's nodes; only the SANs matter
IP=10.32.32.130             # what --internal-ip reports, i.e. what the API server dials

openssl req -new -newkey rsa:2048 -nodes \
  -keyout tls.key -out tls.csr \
  -subj "/O=system:nodes/CN=system:node:${NODE}" \
  -addext "subjectAltName=IP:${IP}"

cat <<EOF | kubectl apply -f -
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: kubezun-${TENANT}
spec:
  request: $(base64 -w0 < tls.csr)
  signerName: kubernetes.io/kubelet-serving
  usages: [digital signature, server auth]
EOF

kubectl certificate approve kubezun-${TENANT}
kubectl get csr kubezun-${TENANT} -o jsonpath='{.status.certificate}' | base64 -d > tls.crt
```

The subject has to be `O=system:nodes, CN=system:node:<name>`: the signer
refuses anything else. It is only an approval-policy artefact — the API server
checks the SANs against the address it dialled, not the CN — so which of the
tenant's nodes is named makes no difference.

The client CA is the cluster's, since the caller being verified is the API
server:

```sh
kubectl config view --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
  | base64 -d > client-ca.crt
```

Then:

```
--tls-cert-file        /etc/kubezun/<TENANT>/tls.crt
--tls-private-key-file /etc/kubezun/<TENANT>/tls.key
--client-ca-file       /etc/kubezun/<TENANT>/client-ca.crt
```

The key is `0600` and its directory `0700`: anyone who can read it can serve the
tenant's kubelet API, which is logs and exec into that tenant's containers.

## Without a certificate

The endpoint does not run and `kubectl logs`/`exec` fail with no route to the
node. That is a working configuration for a cluster that does not offer them
yet; what is refused is a certificate with no `--client-ca-file`, because that
would serve logs and exec to anyone who reached the port.

## What the node-scoped authorizer does and does not do

`kubectl logs` is authorized twice, against two different identities. The API
server checks the tenant for `pods/log` in their namespace, then dials the
kubelet using **its own** client certificate, and the endpoint checks *that*
identity for `nodes/<name>/log`.

So the per-node authorizer bounds which nodes a holder of API-server kubelet
credentials may reach. It is not what keeps one tenant out of another's logs —
that is the API server's own namespace RBAC, which kubezoo drives.

## Renewal

A `kubelet-serving` certificate is short-lived. Nothing here rotates it, so
until the deployment does, treat expiry as an outage of logs and exec — not of
the node, which keeps running: the certificate is only for the kubelet API.
