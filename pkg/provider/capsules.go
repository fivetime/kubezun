package provider

import (
	"context"

	"github.com/fivetime/kubezun/pkg/zun"
)

// Capsules hands the provider the capsule API a namespace's work belongs in.
//
// Two shapes of question, and they are different on purpose. Everything done
// to one pod knows its namespace, so it resolves a tenant's API and acts
// there. The loops — status sync, orphan sweep, stats, token refresh — ask
// "everything this process manages", and under several tenants that is not one
// listing but one per tenant: a capsule listing is scoped to the project the
// credential authenticated as, and no credential can see two projects.
type Capsules interface {
	// For returns the capsule API of the namespace's tenant. It fails closed:
	// a namespace that cannot be resolved gets an error, never somebody
	// else's API.
	For(ctx context.Context, namespace string) (*zun.CapsuleAPI, error)

	// Each visits every tenant's capsule API once, naming the tenant so the
	// caller can tell "this tenant answered nothing" from "this tenant was
	// never visited".
	//
	// ⚠️ That distinction is load-bearing, not bookkeeping. The status sync
	// reads a pod's absence from the listings as "its capsule is gone" and
	// FAILS the pod — so it must never judge a pod whose tenant was skipped.
	// An implementation may skip a tenant it cannot resolve (one bad
	// credential must not starve every other tenant of status updates), but
	// only because callers can see, through the tenant name, whose pods were
	// covered. fn's own error still ends the walk: that is the caller saying
	// stop.
	Each(ctx context.Context, fn func(tenant string, api *zun.CapsuleAPI) error) error

	// TenantOf names the tenant a namespace belongs to, for matching pods
	// against what Each covered. False means the namespace is not served.
	TenantOf(namespace string) (string, bool)
}

// StaticCapsules serves every namespace from one API: the single-tenant
// deployment, and the tests. It is what makes the resolver rollout safe to do
// in steps — a process configured for one tenant behaves exactly as before.
type StaticCapsules struct{ API *zun.CapsuleAPI }

func (s StaticCapsules) For(context.Context, string) (*zun.CapsuleAPI, error) {
	return s.API, nil
}

func (s StaticCapsules) Each(ctx context.Context, fn func(string, *zun.CapsuleAPI) error) error {
	return fn("", s.API)
}

func (s StaticCapsules) TenantOf(string) (string, bool) { return "", true }
