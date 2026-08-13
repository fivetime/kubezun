package zun

import (
	"context"
	"sync"

	"github.com/virtual-kubelet/virtual-kubelet/log"

	"github.com/fivetime/kubezun/pkg/tenant"
)

// ResolvedCapsules serves each namespace from its own tenant's capsule API,
// resolved through the credential resolver. It is the multi-tenant counterpart
// of provider.StaticCapsules.
type ResolvedCapsules struct {
	// Resolver hands out sessions per namespace and knows which tenants this
	// process serves.
	Resolver *tenant.Resolver
	// Tenants lists the distinct tenants currently served, for the walks.
	// Kept separate from the resolver because the resolver answers questions
	// about namespaces, and "who is there to walk" is a question about the
	// namespace WATCH — the thing that already knows the set.
	Tenants func() []string
	// NamespaceOf gives one namespace of a tenant, for resolving that
	// tenant's session. Any namespace of the tenant works: they share one
	// session by construction.
	NamespaceOf func(tenant string) (string, bool)

	// Tenant resolution for the covered check, same source the resolver uses.
	TenantOfNamespace func(namespace string) (string, bool)

	mu   sync.Mutex
	apis map[string]*CapsuleAPI // keyed by tenant
}

// TenantOf names the tenant a namespace belongs to.
func (r *ResolvedCapsules) TenantOf(namespace string) (string, bool) {
	return r.TenantOfNamespace(namespace)
}

// For returns the capsule API of the namespace's tenant.
func (r *ResolvedCapsules) For(ctx context.Context, namespace string) (*CapsuleAPI, error) {
	session, err := r.Resolver.For(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return r.apiFor(session)
}

// apiFor caches one capsule API per session. The session is the cache key
// because the resolver already guarantees one session per tenant, so tenant
// identity rides on pointer identity and needs no second bookkeeping here.
func (r *ResolvedCapsules) apiFor(session *tenant.Session) (*CapsuleAPI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.apis == nil {
		r.apis = map[string]*CapsuleAPI{}
	}
	key := session.Project()
	if api, ok := r.apis[key]; ok {
		return api, nil
	}
	api, err := NewCapsuleAPI(session)
	if err != nil {
		return nil, err
	}
	r.apis[key] = api
	return api, nil
}

// Each visits every served tenant's capsule API once.
//
// ⚠️ A tenant that cannot be resolved is logged and skipped rather than ending
// the walk: the loops behind this reconcile every tenant on a timer, and one
// tenant's bad credential must not starve the rest of status updates. The
// caller's own error still ends the walk — that is it saying stop.
//
// ⚠️ Skipping is only safe because the walk NAMES each tenant it does visit:
// the status sync fails pods that are absent from the listings, so it refuses
// to judge any pod whose tenant is not in what this walk covered. Dropping the
// tenant name from this contract turns a skipped tenant into a mass pod
// failure — measured reasoning, not caution: that is exactly how the sync
// reads absence (sync.go, the covered check).
func (r *ResolvedCapsules) Each(ctx context.Context, fn func(tenant string, api *CapsuleAPI) error) error {
	for _, t := range r.Tenants() {
		namespace, ok := r.NamespaceOf(t)
		if !ok {
			continue // its last namespace vanished between listing and now
		}
		session, err := r.Resolver.For(ctx, namespace)
		if err != nil {
			log.G(ctx).WithError(err).WithField("tenant", t).
				Warn("skipping this tenant's walk: no usable credential")
			continue
		}
		api, err := r.apiFor(session)
		if err != nil {
			log.G(ctx).WithError(err).WithField("tenant", t).
				Warn("skipping this tenant's walk: could not build its capsule endpoint")
			continue
		}
		if err := fn(t, api); err != nil {
			return err
		}
	}
	return nil
}
