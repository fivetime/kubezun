package netpol

import (
	"sync"

	"context"
	"fmt"
	"sort"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	networkingv1listers "k8s.io/client-go/listers/networking/v1"
)

// Reconciler decides which security groups a pod's port should carry, and
// keeps the peer sets a policy refers to up to date.
type Reconciler struct {
	Neutron *Neutron

	Pods      corev1listers.PodLister
	Policies  networkingv1listers.NetworkPolicyLister
	Namespace corev1listers.NamespaceLister

	// ServesNamespace is the authorization boundary, as everywhere else here.
	ServesNamespace func(namespace string) bool

	// PortOf finds the Neutron port a pod's capsule was given. Returns "" when
	// the pod has no capsule yet, which is not an error -- it is a pod that has
	// not been placed.
	PortOf func(pod *corev1.Pod) string

	baseline struct {
		ingress, egress, denyAll string
	}

	// ruleGroups is policy key → its rules-group id, written by the policy
	// worker (SyncPolicyPeers) and only read on the pod path. This is what
	// keeps Neutron off the pod hot path: before it, every pod event paid one
	// read per policy selecting it, and the pod queue has no coalescing.
	ruleMu     sync.Mutex
	ruleGroups map[string]string
}

// ErrPolicyPending says a pod's policy has not been through the policy worker
// yet, so its group id is unknown.
//
// ⚠️ The caller must requeue and WAIT, never fall back to creating the group
// inline: the fallback path is the hot path this cache exists to close, and a
// fallback that "only happens on misses" is the hot path again the first time
// the cache is cold for a burst of pods.
type ErrPolicyPending struct{ Policy string }

func (e ErrPolicyPending) Error() string {
	return "policy " + e.Policy + " has not been prepared yet; the pod will be retried"
}

// rememberRuleGroup records what the policy worker built.
func (r *Reconciler) rememberRuleGroup(policyKey, groupID string) {
	r.ruleMu.Lock()
	defer r.ruleMu.Unlock()
	if r.ruleGroups == nil {
		r.ruleGroups = map[string]string{}
	}
	r.ruleGroups[policyKey] = groupID
}

// ForgetPolicy drops a deleted policy's entry; the sweep owns its groups.
func (r *Reconciler) ForgetPolicy(policyKey string) {
	r.ruleMu.Lock()
	defer r.ruleMu.Unlock()
	delete(r.ruleGroups, policyKey)
}

func (r *Reconciler) ruleGroupOf(policyKey string) (string, bool) {
	r.ruleMu.Lock()
	defer r.ruleMu.Unlock()
	id, ok := r.ruleGroups[policyKey]
	return id, ok
}

// EnsureBaseline resolves the two constant groups once, at startup.
func (r *Reconciler) EnsureBaseline(ctx context.Context) error {
	ingress, egress, err := r.Neutron.EnsureBaseline(ctx)
	if err != nil {
		return err
	}
	denyAll, err := r.Neutron.EnsureDenyAll(ctx)
	if err != nil {
		return err
	}
	r.baseline.ingress, r.baseline.egress, r.baseline.denyAll = ingress, egress, denyAll
	return nil
}

// GroupsFor is the security group list a pod's port should carry.
//
// ⚠️ The whole of the isolation semantic lives in this function. A pod no
// policy selects carries both baseline groups and can talk in every direction,
// because that is what Kubernetes means by "no policy applies" and Neutron's
// own default is the opposite. A policy naming Ingress takes the ingress group
// away, and nothing else changes -- which is why there are two groups and not
// one.
func (r *Reconciler) GroupsFor(ctx context.Context, pod *corev1.Pod) ([]string, error) {
	if r.baseline.ingress == "" || r.baseline.egress == "" || r.baseline.denyAll == "" {
		return nil, fmt.Errorf("the baseline security groups have not been resolved yet")
	}
	policies, err := r.policiesFor(pod.Namespace)
	if err != nil {
		return nil, err
	}
	isolated, err := IsolationOf(pod, policies)
	if err != nil {
		return nil, err
	}
	if len(isolated) == 0 {
		// No policy selects it, so there is nothing to allow and nothing to
		// deny. Skipping the rule-set work here is not an optimisation: it is
		// what keeps a tenant with no policies from creating security group
		// objects, which is the expensive event in the whole design.
		return []string{r.baseline.denyAll, r.baseline.ingress, r.baseline.egress}, nil
	}
	// ⚠️ Always present, so the list is never empty. Neutron reads a port with
	// no groups as "use the project default", which is permissive -- so a pod
	// isolated in both directions would come up reachable by everything, which
	// is the exact opposite of what its policy asked for.
	groups := []string{r.baseline.denyAll}
	if len(isolated[Ingress]) == 0 {
		groups = append(groups, r.baseline.ingress)
	}
	if len(isolated[Egress]) == 0 {
		groups = append(groups, r.baseline.egress)
	}

	allowed, err := r.policyGroupsFor(ctx, pod, policies)
	if err != nil {
		return nil, err
	}
	groups = append(groups, allowed...)
	sort.Strings(groups)
	return groups, nil
}

// ruleSetFor makes sure the group carrying this pod's allowances exists, along
// with the peer sets its rules refer to, and returns its id.
//
// Returns "" when the policies allow nothing, which is the deny-all case: the
// anchor group already says that, and creating an empty group to say it again
// would cost a cloud-wide northd recompute for no effect.
// policyGroupsFor is one security group per policy selecting this pod.
//
// ⚠️ Per policy rather than per pod. Security group rules are an allow-list, so
// carrying several groups means the union of their rules -- which is exactly
// what Kubernetes means by a pod selected by several policies. One group per
// policy therefore composes correctly, and keeps the number of groups equal to
// the number of policies: a pod joining or leaving a policy is then a port
// update, not a new group. Per pod it would be one group per distinct
// COMBINATION of policies, and every new combination is a group creation --
// which is the cloud-wide northd recompute (DESIGN §7.7.3).
func (r *Reconciler) policyGroupsFor(ctx context.Context, pod *corev1.Pod, policies []*networkingv1.NetworkPolicy) ([]string, error) {
	var out []string
	for _, p := range policies {
		t, err := Translate(p)
		if err != nil {
			return nil, err
		}
		if !t.Subject.Matches(labels.Set(pod.Labels)) {
			continue
		}
		set := PolicyRules(t)
		if set.Empty() {
			// A policy that isolates and allows nothing -- the deny-all
			// everyone writes first. The isolation is already expressed by
			// removing a baseline group; an empty security group to say the
			// same thing again would cost a northd recompute for no effect.
			continue
		}

		// ⚠️ Read-only. The groups and their peer sets are built by the policy
		// worker (SyncPolicyPeers); this path pays no Neutron call at all.
		// Before the cache, every pod event paid one read per selecting
		// policy on a queue with no coalescing -- the hottest path in the
		// serverless line doing idempotent bookkeeping that belongs to the
		// policy dimension (2026-08-14 review, accepted).
		id, ok := r.ruleGroupOf(p.Namespace + "/" + p.Name)
		if !ok {
			return nil, ErrPolicyPending{Policy: p.Namespace + "/" + p.Name}
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *Reconciler) policiesFor(namespace string) ([]*networkingv1.NetworkPolicy, error) {
	if r.Policies == nil {
		return nil, nil
	}
	// Every policy in the namespace: the subject selector inside each one
	// decides which pods it applies to, which is not a question a lister can
	// answer.
	return r.Policies.NetworkPolicies(namespace).List(labels.Everything())
}

// AddressesFor is the set of pod addresses a peer selector currently resolves
// to, across the namespaces this process serves.
//
// ⚠️ A pod with no address yet contributes nothing rather than an empty string:
// an empty entry in an address group is rejected by Neutron, and one unplaced
// pod would then stop the whole set from converging.
func (r *Reconciler) AddressesFor(key string, sel PeerSelector) ([]string, error) {
	var out []string
	namespaces, err := r.namespacesMatching(sel)
	if err != nil {
		return nil, err
	}
	for _, ns := range namespaces {
		pods, err := r.Pods.Pods(ns).List(sel.Pods)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			if pod.Status.PodIP == "" {
				continue
			}
			out = append(out, pod.Status.PodIP)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PeerSelector is a resolved peer: which namespaces, and which pods in them.
type PeerSelector struct {
	// Namespace is the one namespace to look in, set when the policy named no
	// namespace selector at all. ⚠️ That case means the policy's OWN namespace,
	// not every namespace, and the two are only distinguishable here.
	Namespace string
	// Namespaces selects namespaces when the policy gave a selector. Only
	// consulted when Namespace is empty.
	Namespaces labels.Selector
	Pods       labels.Selector
}

func (r *Reconciler) namespacesMatching(sel PeerSelector) ([]string, error) {
	if sel.Namespace != "" {
		if !r.ServesNamespace(sel.Namespace) {
			return nil, nil
		}
		return []string{sel.Namespace}, nil
	}
	if r.Namespace == nil {
		return nil, nil
	}
	match := sel.Namespaces
	if match == nil {
		match = labels.Everything()
	}
	list, err := r.Namespace.List(match)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ns := range list {
		if !r.ServesNamespace(ns.Name) {
			continue
		}
		out = append(out, ns.Name)
	}
	sort.Strings(out)
	return out, nil
}

// SyncPolicyPeers makes the address groups one policy refers to hold exactly
// the pods its peer selectors currently match.
//
// ⚠️ Keyed by the policy, and triggered by ANY pod in a namespace this process
// serves -- because a peer is a pod the policy does not select, so nothing
// about the pod itself says which policies care about it. Working that out
// backwards is more state than recomputing the sets, and the sets are what has
// to be right.
func (r *Reconciler) SyncPolicyPeers(ctx context.Context, p *networkingv1.NetworkPolicy) error {
	t, err := Translate(p)
	if err != nil {
		return err
	}
	r.ReportRefusals(ctx, p, t.Refused)
	set := PolicyRules(t)
	keys := make([]string, 0, len(set.Peers))
	for key := range set.Peers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	peers := make(map[string]string, len(set.Peers))
	for _, key := range keys {
		id, err := r.Neutron.EnsureAddressGroup(ctx, AddressGroupName(key))
		if err != nil {
			return err
		}
		addresses, err := r.AddressesFor(key, set.Peers[key])
		if err != nil {
			return err
		}
		if err := r.Neutron.SyncAddresses(ctx, id, addresses); err != nil {
			return err
		}
		peers[key] = id
	}

	// The rules group too: this worker is where every Neutron write of a
	// policy now lives, so the pod path can be a cache read. ⚠️ Recorded only
	// after the write succeeds -- recording first would hand pods a group id
	// that may not exist.
	if set.Empty() {
		r.rememberRuleGroup(p.Namespace+"/"+p.Name, "")
		return nil
	}
	id, err := r.Neutron.EnsureRuleSet(ctx,
		GroupNameFor(p.Namespace, p.Name), p.Namespace+"/"+p.Name, set, peers)
	if err != nil {
		return err
	}
	r.rememberRuleGroup(p.Namespace+"/"+p.Name, id)
	return nil
}

// ReportRefusals writes what a policy asked for and did not get, so it is
// visible where the tenant will look.
//
// ⚠️ This is the only refusal kubezun can make. It is not in the admission
// path, and a NetworkPolicy has no status to write, so a policy naming
// something unsupported is accepted by the apiserver whatever we do. Rejecting
// it outright needs a validating webhook, which lives elsewhere (§7.7.4).
func (r *Reconciler) ReportRefusals(ctx context.Context, p *networkingv1.NetworkPolicy, refused []Refusal) {
	if len(refused) == 0 {
		return
	}
	log.G(ctx).WithField("policy", p.Namespace+"/"+p.Name).
		Warn("parts of this NetworkPolicy are not enforced: " + RefusalMessage(refused))
}
