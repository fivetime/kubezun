# Keeping cluster-wide DaemonSets off virtual nodes

A KNaaS virtual node is a Node object like any other, so the DaemonSet
controller creates a pod for it. Cluster agents tolerate every taint —
cilium's DaemonSets carry `tolerations: [{operator: Exists}]` — so the tenant
taint does not hold them back.

What follows is worse than a few stray pods. The provider refuses them, since
`kube-system` is not a namespace it serves, and they sit in `ProviderFailed`.
Cilium then marks the node `node.cilium.io/agent-not-ready:NoSchedule` because
its agent never became ready there, **and that taint keeps the tenant's own
pods off their own node**. One unrelated DaemonSet takes the node out of
service.

Every virtual node carries `type=virtual-kubelet` (set by virtual-kubelet
itself), which is what these DaemonSets are excluded by.

## Applying it

```bash
kubectl -n kube-system patch ds cilium --type=merge -p '{"spec":{"template":{"spec":{"affinity":{"nodeAffinity":{
  "requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[
    {"key":"type","operator":"NotIn","values":["virtual-kubelet"]}]}]}}}}}}}'
```

Repeat for every cluster-wide DaemonSet. Two details decide whether this works:

- **A DaemonSet that already has a `nodeAffinity` must have the expression
  appended, not replaced.** `cilium-envoy` excludes `cilium.io/no-schedule`
  this way, and a merge patch would drop that. Expressions inside one term are
  ANDed, so appending is the correct composition:

  ```bash
  kubectl -n kube-system patch ds cilium-envoy --type=json -p \
    '[{"op":"add","path":"/spec/template/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/0/matchExpressions/-",
       "value":{"key":"type","operator":"NotIn","values":["virtual-kubelet"]}}]'
  ```

- **A taint left behind is not removed by this.** `nodeAffinity` stops future
  pods; a node already marked `agent-not-ready` stays marked, because the agent
  that would clear it is no longer scheduled there. Remove it once:

  ```bash
  kubectl taint node <virtual-node> node.cilium.io/agent-not-ready-
  ```

## Verifying

`kubectl get ds -A` should show DESIRED counting only real nodes, and
`kubectl get pods -A --field-selector spec.nodeName=<virtual-node>` should be
empty. A tenant pod should then schedule carrying nothing but its own
`knaas.io/tenant` toleration.

## Which DaemonSets need this

Only those that tolerate the tenant taint. In the development cluster that was
`cilium`, `cilium-envoy` and `cilium-node-init`; `konnectivity-agent` and
`ovn-chassis` never targeted the virtual node. Check with:

```bash
kubectl get ds -A -o json | jq -r '.items[] |
  select(.spec.template.spec.tolerations[]? | select(.operator=="Exists" and (.key//"")=="")) |
  "\(.metadata.namespace)/\(.metadata.name)"'
```

On a cluster where these workloads are managed by an operator, patching the
DaemonSet directly will be reverted; set the affinity through the operator's
own configuration, or enforce it with a mutating policy so it survives
reconciliation.
