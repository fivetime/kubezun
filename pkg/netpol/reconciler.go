package netpol

import (
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
		ingress, egress string
	}
}

// EnsureBaseline resolves the two constant groups once, at startup.
func (r *Reconciler) EnsureBaseline(ctx context.Context) error {
	ingress, egress, err := r.Neutron.EnsureBaseline(ctx)
	if err != nil {
		return err
	}
	r.baseline.ingress, r.baseline.egress = ingress, egress
	return nil
}

// BaselineGroups are the two groups every pod carries until a policy takes one
// away. A capsule must be created with them: a pod that arrives without them
// is a pod nothing can reach, and one that arrives carrying only the project
// default is a pod that stops being reachable the moment the tenant is
// converted (§7.7.5a).
func (r *Reconciler) BaselineGroups() []string {
	return []string{r.baseline.ingress, r.baseline.egress}
}

// GroupsFor is the security group list a pod's port should carry.
//
// ⚠️ The whole of the isolation semantic lives in this function. A pod no
// policy selects carries both baseline groups and can talk in every direction,
// because that is what Kubernetes means by "no policy applies" and Neutron's
// own default is the opposite. A policy naming Ingress takes the ingress group
// away, and nothing else changes -- which is why there are two groups and not
// one.
func (r *Reconciler) GroupsFor(pod *corev1.Pod) ([]string, error) {
	if r.baseline.ingress == "" || r.baseline.egress == "" {
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
	var groups []string
	if len(isolated[Ingress]) == 0 {
		groups = append(groups, r.baseline.ingress)
	}
	if len(isolated[Egress]) == 0 {
		groups = append(groups, r.baseline.egress)
	}
	// ⚠️ Not an empty list. Neutron injects the project's default group when
	// the attribute is absent, and an empty list is a different thing from an
	// unset one -- but a port with no groups at all is also a port whose
	// isolation nothing can later relax. A pod isolated in both directions
	// carries the rule-set groups its policies produced, and until those exist
	// it carries nothing, which is deny-all: the safe direction.
	sort.Strings(groups)
	return groups, nil
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
