package main

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/client-go/kubernetes"

	kingress "github.com/fivetime/kubezun/pkg/ingress"
	"github.com/fivetime/kubezun/pkg/netpol"
	"github.com/fivetime/kubezun/pkg/provider"
	kservice "github.com/fivetime/kubezun/pkg/service"
	"github.com/fivetime/kubezun/pkg/tenant"
	vkset "github.com/fivetime/kubezun/pkg/vknode"
	kvolume "github.com/fivetime/kubezun/pkg/volume"
	"github.com/fivetime/kubezun/pkg/zun"
)

// multiTenant is the glue between the credential resolver and the per-tenant
// reconciler seams. Each factory clones the process's template reconciler and
// swaps exactly the parts that belong to a tenant: the OpenStack clients, the
// tenant name, and — load-bearing, not bookkeeping — the namespace check.
//
// ⚠️ The namespace check is the one that must not be shared. A per-tenant
// netpol reconciler resolves policy peers across "all served namespaces"; with
// the process-wide check that is every tenant's namespaces, and tenant A's
// address groups would fill with tenant B's pod addresses — B's pods allowed
// through A's policies. Scoped per tenant, "everywhere" means everywhere in
// that tenant, which is what the API's namespaceSelector semantics promise.
type multiTenant struct {
	resolver *tenant.Resolver
	watcher  *vkset.Namespaces

	// Templates: copied per tenant, never used to serve traffic themselves.
	volume  *kvolume.Reconciler
	service *kservice.Reconciler
	ingress *kingress.Reconciler
	netpol  *netpol.Reconciler

	mu        sync.Mutex
	volumes   map[string]*kvolume.Reconciler
	services  map[string]*kservice.Reconciler
	ingresses map[string]*kingress.Reconciler
	netpols   map[string]*netpol.Reconciler
}

func newMultiTenant(client kubernetes.Interface, watcher *vkset.Namespaces, platformNS string) *multiTenant {
	return &multiTenant{
		resolver: &tenant.Resolver{
			Secrets:  client.CoreV1().Secrets(platformNS),
			TenantOf: watcher.TenantOf,
		},
		watcher:   watcher,
		volumes:   map[string]*kvolume.Reconciler{},
		services:  map[string]*kservice.Reconciler{},
		ingresses: map[string]*kingress.Reconciler{},
		netpols:   map[string]*netpol.Reconciler{},
	}
}

// servesTenant bounds one tenant's reconciler to that tenant's namespaces.
func (m *multiTenant) servesTenant(t string) func(string) bool {
	return func(namespace string) bool {
		got, ok := m.watcher.TenantOf(namespace)
		return ok && got == t
	}
}

// binding resolves a namespace to its tenant and binding, in one place so
// every factory refuses the same way.
func (m *multiTenant) binding(ctx context.Context, namespace string) (string, *tenant.Binding, error) {
	t, ok := m.watcher.TenantOf(namespace)
	if !ok || t == "" {
		return "", nil, fmt.Errorf("namespace %q names no tenant", namespace)
	}
	b, err := m.resolver.BindingFor(ctx, namespace)
	if err != nil {
		return "", nil, err
	}
	return t, b, nil
}

// capsules is the provider's view: per-namespace capsule APIs.
func (m *multiTenant) capsules() *zun.ResolvedCapsules {
	return &zun.ResolvedCapsules{
		Resolver: m.resolver,
		Tenants:  m.watcher.Tenants,
		NamespaceOf: func(t string) (string, bool) {
			if nss := m.watcher.NamespacesOfTenant(t); len(nss) > 0 {
				return nss[0], true
			}
			return "", false
		},
		TenantOfNamespace: m.watcher.TenantOf,
	}
}

// networkIDFor resolves the capsule network per namespace, from the tenant's
// binding rather than the process flag.
func (m *multiTenant) networkIDFor(ctx context.Context, namespace string) (string, error) {
	_, b, err := m.binding(ctx, namespace)
	if err != nil {
		return "", err
	}
	if b.NetworkID == "" {
		return "", fmt.Errorf("namespace %q: its tenant's binding names no network "+
			"(annotate the credential Secret with %s)", namespace, tenant.NetworkIDAnnotation)
	}
	return b.NetworkID, nil
}

// eachTenant runs fn once per served tenant that resolves, skipping the ones
// that do not — same contract as ResolvedCapsules.Each, and safe for the same
// reason: every walk behind this treats "not visited" as "leave it alone",
// never as "gone".
func eachTenant[R any](ctx context.Context, m *multiTenant, forNS func(context.Context, string) (R, error), fn func(R) error) error {
	for _, t := range m.watcher.Tenants() {
		nss := m.watcher.NamespacesOfTenant(t)
		if len(nss) == 0 {
			continue
		}
		r, err := forNS(ctx, nss[0])
		if err != nil {
			continue // this tenant this round; the sweep comes back
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiTenant) volumeFor(ctx context.Context, namespace string) (*kvolume.Reconciler, error) {
	t, b, err := m.binding(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if m.volume == nil {
		// The template is set where the controller is built; a nil one here is
		// that controller being disabled (no VIP subnet, storage off, …).
		return nil, fmt.Errorf("the volume controller is not running in this process")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.volumes[t]; ok {
		return r, nil
	}
	blockC, err := kvolume.NewBlockStorageClient(b.Session)
	if err != nil {
		blockC = nil // same posture as startup: refused claims, not a dead process
	}
	sharedC, err := kvolume.NewSharedFSClient(b.Session)
	if err != nil {
		sharedC = nil
	}
	capsules, err := zun.NewCapsuleAPI(b.Session)
	if err != nil {
		return nil, err
	}
	r := *m.volume
	backend := *m.volume.Backend
	backend.Block, backend.Shared = blockC, sharedC
	r.Backend = &backend
	r.Capsules = capsules
	r.Tenant = t
	r.ServesNamespace = m.servesTenant(t)
	m.volumes[t] = &r
	return &r, nil
}

func (m *multiTenant) serviceFor(ctx context.Context, namespace string) (*kservice.Reconciler, error) {
	t, b, err := m.binding(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if m.service == nil {
		// The template is set where the controller is built; a nil one here is
		// that controller being disabled (no VIP subnet, storage off, …).
		return nil, fmt.Errorf("the service controller is not running in this process")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.services[t]; ok {
		return r, nil
	}
	octavia, err := kservice.NewOctaviaClient(b.Session)
	if err != nil {
		return nil, err
	}
	neutron, err := kservice.NewNetworkClient(b.Session)
	if err != nil {
		return nil, err
	}
	capsules, err := zun.NewCapsuleAPI(b.Session)
	if err != nil {
		return nil, err
	}
	r := *m.service
	r.Octavia, r.Neutron = octavia, neutron
	r.Subnets = kservice.NewCapsuleSubnets(capsules)
	r.Tenant = t
	r.ServesNamespace = m.servesTenant(t)
	r.Namespaces = func() []string { return m.watcher.NamespacesOfTenant(t) }
	if b.VIPSubnetID != "" {
		r.VIPSubnetID = b.VIPSubnetID
	}
	if b.VIPNetworkID != "" {
		r.VIPNetworkID = b.VIPNetworkID
	}
	m.services[t] = &r
	return &r, nil
}

func (m *multiTenant) ingressFor(ctx context.Context, namespace string) (*kingress.Reconciler, error) {
	t, b, err := m.binding(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if m.ingress == nil {
		// The template is set where the controller is built; a nil one here is
		// that controller being disabled (no VIP subnet, storage off, …).
		return nil, fmt.Errorf("the ingress controller is not running in this process")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.ingresses[t]; ok {
		return r, nil
	}
	octavia, err := kservice.NewOctaviaClient(b.Session)
	if err != nil {
		return nil, err
	}
	neutron, err := kservice.NewNetworkClient(b.Session)
	if err != nil {
		return nil, err
	}
	capsules, err := zun.NewCapsuleAPI(b.Session)
	if err != nil {
		return nil, err
	}
	r := *m.ingress
	r.Octavia, r.Neutron = octavia, neutron
	r.Subnets = kservice.NewCapsuleSubnets(capsules)
	// Barbican stays optional per tenant, as it is at startup.
	if km, err := kservice.NewKeyManagerClient(b.Session); err == nil {
		r.KeyManager = km
	} else {
		r.KeyManager = nil
	}
	r.Tenant = t
	r.ServesNamespace = m.servesTenant(t)
	r.Namespaces = func() []string { return m.watcher.NamespacesOfTenant(t) }
	if b.VIPSubnetID != "" {
		r.VIPSubnetID = b.VIPSubnetID
	}
	m.ingresses[t] = &r
	return &r, nil
}

func (m *multiTenant) netpolFor(ctx context.Context, namespace string) (*netpol.Reconciler, error) {
	t, b, err := m.binding(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if m.netpol == nil {
		// The template is set where the controller is built; a nil one here is
		// that controller being disabled (no VIP subnet, storage off, …).
		return nil, fmt.Errorf("the netpol controller is not running in this process")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.netpols[t]; ok {
		return r, nil
	}
	netC, err := netpol.NewClient(b.Session)
	if err != nil {
		return nil, err
	}
	r := *m.netpol
	r.Neutron = &netpol.Neutron{Client: netC}
	// ⚠️ The line that keeps tenants apart: peers resolve only within this
	// tenant's namespaces. See the type comment.
	r.ServesNamespace = m.servesTenant(t)
	m.netpols[t] = &r
	return &r, nil
}

// providerCapsules picks the provider's capsule source: resolved per namespace
// under multi-tenancy, the fixed API otherwise.
func providerCapsules(m *multiTenant, api *zun.CapsuleAPI) provider.Capsules {
	if m == nil {
		return provider.StaticCapsules{API: api}
	}
	return m.capsules()
}
