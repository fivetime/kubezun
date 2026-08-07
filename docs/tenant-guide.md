# Running on a KNaaS virtual node

A virtual node is a real Kubernetes node in every way your manifests can see:
it has a name, labels, capacity, taints and an address, the scheduler places
pods on it, and `kubectl get nodes` lists it. What runs behind it is not a
machine you share with anyone — each pod becomes an isolated VM of its own.

That difference is invisible for most workloads and decisive for a few. This is
the list of the few.

## What is not there

**No shared host.** `hostNetwork`, `hostPID`, `hostIPC` and `hostPath` volumes
are refused when the pod is created. There is no host to share: your pod's
network is its own port on your tenant network, and its filesystem is its own.

- Two pods cannot pass a Unix socket or a file through a host directory. Use
  the network — they can reach each other by pod IP, Service, or your tenant's
  DNS.
- A DaemonSet cannot observe its neighbours through `/proc` or a host mount. A
  sidecar in the same pod can, and shares its network namespace.

**No privileged containers.** `securityContext.privileged` is refused. So are
capabilities beyond the default set, by admission policy.

**No `spec.nodeName`.** Pods go through the scheduler. Writing the node name
directly bypasses it and is refused.

## What behaves differently

**ConfigMap and Secret files are a snapshot.** They are read when the pod is
created and travel with it. Editing a ConfigMap afterwards does not change the
file inside a running pod — a real kubelet would eventually refresh it. Restart
the pod to pick up a change. Environment variables from them work normally.

**Only configMap and secret volumes.** `emptyDir`, `downwardAPI` and
PersistentVolumeClaims are refused rather than ignored, so a pod that asks for
one fails at creation instead of running without the files it expects.

**Service account tokens are off by default.** A capsule cannot refresh a bound
token. Set `automountServiceAccountToken: false`, which the tenant policy does
for you. A pod that genuinely needs to call the API server should ask.

**Probes run inside the container.** `httpGet` and `tcpSocket` probes are
rewritten into a command run in your container against `127.0.0.1`, because
nothing outside the pod's VM can reach its address. Two things follow:

- `httpGet.host` and `tcpSocket.host` must be empty or `localhost`; a probe
  aimed elsewhere is refused rather than silently testing the wrong thing.
- Your image needs `curl` or `wget` (for `httpGet`), `nc` or `curl` (for
  `tcpSocket`). A distroless image has neither, and the probe fails with
  `image has no curl or wget`. Use an `exec` probe instead, which needs
  nothing but your own binary.

**`kubectl logs -f` is not available.** Logs are returned whole rather than
streamed. `kubectl logs`, `--tail`, `--since` and `--timestamps` all work.

**`kubectl exec` has no terminal.** A command runs to completion and its output
comes back: `kubectl exec pod -- ls` works, `kubectl exec -it pod -- sh` does
not. There is no attach and no port-forward.

**No `kubectl top`.** Per-pod resource metrics are not collected.

## What to write in a pod

**Set limits.** A container's limits become the VM's real CPU and memory
ceiling. A container with no limits gets a minimum allocation, which is rarely
what you want — set `resources.limits` on anything that matters.

**Set an architecture if you care.** Nodes carry `kubernetes.io/arch` and a
pod is placed only on a node whose architecture matches, all the way down to
the machine that runs it. A pod that needs one should say so with a
`nodeSelector`; one that does not runs anywhere.

**Expect a slower start.** A pod is a VM, so the first start of an image takes
tens of seconds rather than a second or two. Set `initialDelaySeconds` on
liveness probes accordingly — it is honoured, and without it a slow application
can be restarted before it has finished coming up.

## What works exactly as you expect

Deployments, StatefulSets, Jobs, DaemonSets and their rollouts. Services and
Ingress. Readiness gating traffic. Liveness restarting a container, keeping its
IP. HPA on replica count. `kubectl get`, `describe`, `edit`, `scale`, `rollout`.
Your own network policy within your tenant network.

A DaemonSet runs one pod per virtual node you have, which is one per
architecture and availability zone you asked for — not one per physical
machine, since you do not have any.
