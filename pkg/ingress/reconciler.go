package ingress

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/providers"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1client "k8s.io/client-go/kubernetes/typed/networking/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
	networkingv1listers "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/record"

	"github.com/fivetime/kubezun/pkg/service"
)

const (
	// LegacyClassAnnotation is honoured alongside spec.ingressClassName, the
	// way every ingress controller does, because charts written before the
	// IngressClass API still carry it.
	LegacyClassAnnotation = "kubernetes.io/ingress.class"

	// lbIDAnnotation remembers which load balancer belongs to an Ingress, the
	// same contract pkg/service keeps for Services.
	lbIDAnnotation = "knaas.io/loadbalancer-id"

	// The exposure annotations are the upstream octavia-ingress-controller's,
	// verbatim, so existing runbooks and charts transfer unchanged.
	InternalAnnotation       = "octavia.ingress.kubernetes.io/internal"
	FloatingIPAnnotation     = "octavia.ingress.kubernetes.io/floatingip"
	KeepFloatingIPAnnotation = "octavia.ingress.kubernetes.io/keep-floatingip"

	// ProviderAnnotation picks the Octavia provider for one Ingress, over the
	// deployment's default. Without it the provider is a process-level setting,
	// so a tenant could not have one Ingress on a provider that terminates TLS
	// in software and another on one that does not -- they differ in what they
	// can do and in what they cost, and that is a per-Ingress decision.
	//
	// Effectively create-time: Octavia has no operation that moves a live load
	// balancer between providers, so a changed value is refused rather than
	// quietly ignored. See providerMatches.
	ProviderAnnotation = "knaas.io/ingress-provider"

	// ovnProvider is named here only to refuse it. pkg/service builds every
	// Service on it deliberately -- layer 4 is all a ClusterIP needs, and it
	// costs nothing but OVN flows. An Ingress is the one thing it cannot do.
	ovnProvider = "ovn"
)

// resolveProvider returns the Octavia provider for one Ingress: what it asks
// for, else this deployment's default.
//
// "ovn" is refused outright. It is the provider every Service here uses, so it
// is the one a tenant is most likely to reach for -- and it is L4-only: it
// answers UnsupportedOptionError to the TERMINATED_HTTPS listener and every
// l7policy this builds. Accepting it would produce a load balancer with no
// listener and a failure several API calls away from the annotation that
// caused it.
func (r *Reconciler) resolveProvider(ing *networkingv1.Ingress) (string, error) {
	provider := strings.TrimSpace(ing.Annotations[ProviderAnnotation])
	if provider == "" {
		return r.Provider, nil
	}
	if provider == ovnProvider {
		return "", fmt.Errorf("%s=%s: the ovn provider is layer 4 only and cannot serve an Ingress (it refuses TERMINATED_HTTPS listeners and l7policies); name a provider that terminates HTTP, such as amphora",
			ProviderAnnotation, provider)
	}
	return provider, nil
}

// checkProviderInstalled turns a name this deployment does not have into a
// message that says so and lists what it does have.
//
// Without it a typo becomes a load balancer create that fails somewhere inside
// Octavia, retried forever, with the tenant's only clue an error naming the
// string they already wrote. Only called when an Ingress asked for a specific
// provider and no load balancer exists yet, so it costs one call on a path
// taken once.
func checkProviderInstalled(ctx context.Context, octavia *gophercloud.ServiceClient, want string) error {
	pages, err := providers.List(octavia, providers.ListOpts{}).AllPages(ctx)
	if err != nil {
		// Not being able to ask is not evidence the provider is missing; let
		// the create attempt be the judge.
		return nil
	}
	all, err := providers.ExtractProviders(pages)
	if err != nil || len(all) == 0 {
		return nil
	}
	names := make([]string, 0, len(all))
	for i := range all {
		if all[i].Name == want {
			return nil
		}
		names = append(names, all[i].Name)
	}
	return fmt.Errorf("%s=%s: this deployment has no such Octavia provider; it offers %s",
		ProviderAnnotation, want, strings.Join(names, ", "))
}

// providerMatches refuses to reconcile an Ingress whose provider changed under
// a load balancer that already exists.
//
// Octavia cannot move one, so there are only bad options and this picks the
// loud one. Carrying on would leave the Ingress served by the provider its
// annotation no longer names, with everything reporting healthy. Recreating it
// silently would drop the VIP and any floating IP bound to it -- addresses a
// tenant has published, deleted by an annotation edit.
//
// A live load balancer with no provider reported (older Octavia omits it) is
// treated as a match: refusing on missing information would strand every
// Ingress on such a deployment.
func providerMatches(lb *loadbalancers.LoadBalancer, want string) error {
	if lb.Provider == "" || want == "" || lb.Provider == want {
		return nil
	}
	return fmt.Errorf("load balancer %s serves this Ingress on provider %q and %q is now requested: Octavia cannot move a load balancer between providers. Delete the Ingress and create it again to move it -- its address, and any floating IP on it, will change",
		lb.ID, lb.Provider, want)
}

// Reconciler turns one tenant's Ingresses into Octavia L7 load balancers.
//
// The tenant's own clients, the tenant's own scope: this process serves one
// tenant, so unlike the implementation this is adapted from there is no
// per-namespace credential resolution and no sharding — the boundary is the
// served namespace set, enforced by the controller before anything reaches
// here (the same 19-load-balancers lesson pkg/service records).
type Reconciler struct {
	Octavia *gophercloud.ServiceClient
	Neutron *gophercloud.ServiceClient
	// KeyManager is Barbican, needed only for TLS. Nil is legal and refuses
	// TERMINATED_HTTPS with a readable error instead of a nil dereference.
	KeyManager *gophercloud.ServiceClient

	Ingresses     networkingv1listers.IngressLister
	Services      corev1listers.ServiceLister
	Slices        discoveryv1listers.EndpointSliceLister
	IngressClient networkingv1client.IngressesGetter

	// Secrets reads a TLS Secret on demand. A function rather than a lister on
	// purpose: this process caches no Secrets, ever (pkg/vknode/listers.go
	// says why), and a certificate is read only when a reconcile needs it.
	Secrets func(ctx context.Context, namespace, name string) (*corev1.Secret, error)

	// Subnets resolves a member's subnet from its capsule, pkg/service's seam.
	Subnets service.SubnetResolver

	// VIPSubnetID is where the load balancer's address comes from — the same
	// service subnet Services use, not the pod subnet (dst-MAC trap).
	VIPSubnetID string
	// FloatingNetworkID is the external network public addresses come from.
	FloatingNetworkID string

	// Provider is the Octavia provider for Ingress load balancers. NEVER
	// "ovn": that provider is L4-only and refuses every L7 object. This is
	// also why Ingress is a priced capability where a Service is free — an
	// L7 load balancer is real instances, not just OVN flows (DESIGN §7.5a).
	Provider string
	// ClassName is the ingress class this controller answers for; anything
	// else belongs to other controllers.
	ClassName string
	// Tenant scopes object names, so a sweep can recognise its own.
	Tenant string

	// ServesNamespace bounds what this tenant's credential may be spent on.
	ServesNamespace func(namespace string) bool
	// Namespaces lists the served set, so the sweep can tell "serves nothing"
	// from "does not know yet" and refuse to run on the second.
	Namespaces func() []string

	Events record.EventRecorder
}

// lbName derives the stable load balancer name for one Ingress.
//
// The "ing" segment separates these from Service load balancers
// ("kubezun_<tenant>_<ns>_<svc>"). Segment COUNT disambiguates, not the
// literal: a namespace cannot contain an underscore, so a name with three
// underscore-separated tails can only be an Ingress one — which is also what
// keeps the Service sweep's parser from reading "ing" as a namespace and
// deleting every Ingress load balancer as an orphan.
func (r *Reconciler) lbName(namespace, name string) string {
	return fmt.Sprintf("kubezun_%s_ing_%s_%s", r.Tenant, namespace, name)
}

// ParseLBName recovers the Ingress a load balancer belongs to. The sweep and
// pkg/service's use it to divide the world.
func ParseLBName(tenant, lbName string) (namespace, name string, ok bool) {
	prefix := "kubezun_" + tenant + "_ing_"
	if !strings.HasPrefix(lbName, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(lbName, prefix)
	namespace, name, found := strings.Cut(rest, "_")
	if !found || namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

// Ours reports whether an Ingress carries this controller's class.
//
// Two spellings are accepted: the class as configured, and the class behind
// the tenant's prefix. The gateway prefixes tenant-owned cluster-scoped names
// with the tenant id — IngressClass among them — so the tenant writes
// "knaas" and this process reads "111111-knaas". Matching only the bare name
// sent every tenant Ingress down the teardown path as not-ours (caught on
// first deployment).
func (r *Reconciler) Ours(ing *networkingv1.Ingress) bool {
	prefixed := r.Tenant + "-" + r.ClassName
	if cls := ing.Spec.IngressClassName; cls != nil {
		if *cls == r.ClassName || *cls == prefixed {
			return true
		}
	}
	legacy := ing.Annotations[LegacyClassAnnotation]
	return legacy == r.ClassName || legacy == prefixed
}

// Reconcile brings one Ingress's load balancer into line with its spec.
func (r *Reconciler) Reconcile(ctx context.Context, namespace, name string) error {
	ing, err := r.Ingresses.Ingresses(namespace).Get(name)
	if err != nil {
		// Gone (or unreadable, which retries). The load balancer is found by
		// its derived name — there is no object left to read an id off, which
		// is the reason the name is derived and not random.
		return r.tearDownByName(ctx, namespace, name)
	}
	if !r.Ours(ing) {
		// Not this controller's, or the class moved away. If a load balancer
		// exists under our name the class DID move away, and it is torn down
		// rather than left running and billed for an Ingress that now belongs
		// to somebody else.
		return r.tearDownByName(ctx, namespace, name)
	}
	if ing.DeletionTimestamp != nil {
		return r.tearDownByName(ctx, namespace, name)
	}

	lb, err := r.ensureLoadBalancer(ctx, ing)
	if err != nil {
		return err
	}
	lbname := r.lbName(namespace, name)

	// TLS first: the listener needs the refs. Stale generations are removed
	// only after the listener holds the new ones (rotation ordering below).
	tlsRefs, err := r.ensureTLS(ctx, ing, lbname)
	if err != nil {
		return err
	}

	// Pools (+ their HTTP health monitors) before the listener, members after
	// it: a listener whose TLS ref needs repair must never be starved behind
	// member churn (the adapted-from code caught that deadlock live).
	defaultRef, backends := collectBackends(ing)
	poolIDs := map[string]string{}
	keepPools := map[string]bool{}
	for _, b := range backends {
		pool, err := r.ensureBackendPool(ctx, lb, lbname, b)
		if err != nil {
			return err
		}
		poolIDs[poolName(lbname, b)] = pool.ID
		keepPools[pool.ID] = true
	}
	defaultPoolID := ""
	if defaultRef != nil {
		defaultPoolID = poolIDs[poolName(lbname, *defaultRef)]
	}

	tuning, err := tuningFromAnnotations(ing)
	if err != nil {
		return err
	}
	listener, err := ensureListener(ctx, r.Octavia, lb.ID, lbname, tlsRefs, defaultPoolID, tuning)
	if err != nil {
		return err
	}

	// Only now do stale certificate generations go: the listener above holds
	// the new refs, so nothing in Octavia can still need the old ones.
	if len(tlsRefs) > 0 {
		keep := map[string]bool{}
		for _, ref := range tlsRefs {
			keep[ref] = true
		}
		if err := cleanupBarbicanSecrets(ctx, r.KeyManager, lbname+"_tls_", keep); err != nil {
			return err
		}
	}

	for _, b := range backends {
		if err := r.syncPoolMembers(ctx, lb, ing, b, poolIDs[poolName(lbname, b)]); err != nil {
			return err
		}
	}

	desired := buildDesiredPolicies(ing, lbname, poolIDs)
	if err := syncL7Policies(ctx, r.Octavia, lb.ID, listener.ID, lbname, desired); err != nil {
		return err
	}
	if err := gcStalePools(ctx, r.Octavia, lb.ID, lbname, keepPools, defaultPoolID); err != nil {
		return err
	}

	address, err := r.ensureFIP(ctx, ing, lbname, lb.VipPortID)
	if err != nil {
		return err
	}
	if address == "" {
		address = lb.VipAddress
	}
	return r.publishStatus(ctx, ing, address)
}

// ensureLoadBalancer gets-or-creates the Ingress load balancer: by recorded
// id, then by name, then create — pkg/service's shape with the L7 provider.
func (r *Reconciler) ensureLoadBalancer(ctx context.Context, ing *networkingv1.Ingress) (*loadbalancers.LoadBalancer, error) {
	provider, err := r.resolveProvider(ing)
	if err != nil {
		return nil, err
	}

	if id := ing.Annotations[lbIDAnnotation]; id != "" {
		lb, err := service.GetLoadBalancerByID(ctx, r.Octavia, id)
		if err == nil {
			if err := providerMatches(lb, provider); err != nil {
				return nil, err
			}
			return service.WaitActive(ctx, r.Octavia, lb.ID)
		}
		if err != service.ErrNotFound {
			return nil, fmt.Errorf("reading load balancer %s: %w", id, err)
		}
		// Deleted behind our back; fall through and make another.
	}

	name := r.lbName(ing.Namespace, ing.Name)
	lb, err := service.GetLoadBalancerByName(ctx, r.Octavia, name)
	switch {
	case err == nil:
		if err := providerMatches(lb, provider); err != nil {
			return nil, err
		}
	case err == service.ErrNotFound:
		if ing.Annotations[ProviderAnnotation] != "" {
			if err := checkProviderInstalled(ctx, r.Octavia, provider); err != nil {
				return nil, err
			}
		}
		lb, err = loadbalancers.Create(ctx, r.Octavia, loadbalancers.CreateOpts{
			Name:        name,
			Description: fmt.Sprintf("kubezun Ingress %s/%s", ing.Namespace, ing.Name),
			VipSubnetID: r.VIPSubnetID,
			Provider:    provider,
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("creating load balancer %q (provider=%s): %w", name, provider, err)
		}
	default:
		return nil, err
	}

	lb, err = service.WaitActive(ctx, r.Octavia, lb.ID)
	if err != nil {
		return nil, err
	}
	if ing.Annotations[lbIDAnnotation] != lb.ID {
		updated := ing.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[lbIDAnnotation] = lb.ID
		if _, err := r.IngressClient.Ingresses(ing.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}
	return lb, nil
}

// ensureTLS mirrors every spec.tls Secret into Barbican and returns the refs
// in spec order (first = listener default, rest = SNI).
//
// The division of labour is the one settled for this platform: the tenant
// issues and renews certificates however they like; this only carries what the
// Secret holds. Rotation is a content-hash rename inside ensureBarbicanSecret,
// and the stale generation is removed by the caller only after the listener
// has been re-pointed.
func (r *Reconciler) ensureTLS(ctx context.Context, ing *networkingv1.Ingress, lbname string) ([]string, error) {
	if len(ing.Spec.TLS) == 0 {
		return nil, nil
	}
	prefix := lbname + "_tls_"
	var refs []string
	seen := map[string]bool{}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == "" || seen[t.SecretName] {
			continue
		}
		seen[t.SecretName] = true
		sec, err := r.Secrets(ctx, ing.Namespace, t.SecretName)
		if err != nil {
			return nil, fmt.Errorf("reading TLS Secret %s: %w", t.SecretName, err)
		}
		ref, err := ensureBarbicanSecret(ctx, r.KeyManager, prefix+t.SecretName, sec)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// publishStatus writes the reachable address into the Ingress status, which is
// where kubectl get ingress reads ADDRESS from.
func (r *Reconciler) publishStatus(ctx context.Context, ing *networkingv1.Ingress, address string) error {
	for _, s := range ing.Status.LoadBalancer.Ingress {
		if s.IP == address {
			return nil
		}
	}
	updated := ing.DeepCopy()
	updated.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: address}}
	_, err := r.IngressClient.Ingresses(ing.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

// tearDownByName removes everything an Ingress created, found by derived name.
//
// No finalizer, deliberately. pkg/service's recovery model is name-derivation
// plus a sweep, and one model for both kinds of load balancer means one set of
// failure modes: anything this misses (process down at the wrong moment) is
// the sweep's to reclaim, exactly as for Services.
func (r *Reconciler) tearDownByName(ctx context.Context, namespace, name string) error {
	lbname := r.lbName(namespace, name)
	lb, err := service.GetLoadBalancerByName(ctx, r.Octavia, lbname)
	if err == service.ErrNotFound {
		// Nothing was ever created (the common case: an Ingress of another
		// class deleted, or one we never got to). Barbican might still hold
		// bundles if the load balancer went first — best-effort ONLY: a
		// plain-HTTP tenant's credential may lack key-manager access
		// entirely, and a 403 here retried forever wedges the whole queue
		// behind an Ingress that owns nothing (caught on first deployment).
		if err := cleanupBarbicanSecrets(ctx, r.KeyManager, lbname+"_tls_", nil); err != nil {
			log.G(ctx).WithError(err).WithField("ingress-lb", lbname).
				Debug("skipping Barbican cleanup for an Ingress that owns no load balancer")
		}
		return nil
	}
	if err != nil {
		return err
	}
	return r.tearDown(ctx, lbname, lb)
}

// tearDown is the shared teardown: floating IP, load balancer, certificates.
func (r *Reconciler) tearDown(ctx context.Context, lbname string, lb *loadbalancers.LoadBalancer) error {
	if err := r.releaseFIP(ctx, lbname, lb.VipPortID); err != nil {
		return err
	}
	log.G(ctx).WithField("ingress-lb", lb.ID).Info("deleting ingress load balancer")
	if err := service.DeleteLoadBalancer(ctx, r.Octavia, lb.ID); err != nil {
		return err
	}
	// Barbican cleanup after the load balancer: while it exists a listener may
	// still reference a bundle, and deleting a referenced bundle wedges the
	// provider in PENDING_UPDATE (caught live in the code this is adapted
	// from). Best-effort when there was no TLS: a plain-HTTP tenant's
	// credential may legitimately lack key-manager access.
	if err := cleanupBarbicanSecrets(ctx, r.KeyManager, lbname+"_tls_", nil); err != nil {
		log.G(ctx).WithError(err).WithField("ingress-lb", lbname).
			Warn("could not clean Barbican bundles; they are the sweep's to retry")
	}
	return nil
}
