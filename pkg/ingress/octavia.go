// Package ingress turns a tenant's Ingress objects into Octavia L7 load
// balancers, the way pkg/service does for Services at L4.
//
// Adapted from kubetron's pkg/ingress, which runs one controller for many
// tenants and so resolves per-namespace credentials, shards namespaces, and
// guards teardown with finalizers. None of that survives here: this process IS
// one tenant — its credential is the process's own, its scope is the served
// namespace set, and teardown follows pkg/service's shape (name-derived delete
// plus an orphan sweep) rather than finalizers, so one recovery model covers
// both kinds of load balancer.
package ingress

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"

	"github.com/fivetime/kubezun/pkg/service"
)

// The helpers below are the L7 vocabulary pkg/service never needed: pools
// looked up by name rather than by listener, listener updates (certificate
// rotation re-points a ref in place), and the l7policy/l7rule objects that
// carry host/path routing. Each waits for the load balancer to settle, for the
// same reason pkg/service's do — Octavia refuses to touch a load balancer that
// is still provisioning.

// getPoolByName finds a pool of one load balancer by exact name.
func getPoolByName(ctx context.Context, c *gophercloud.ServiceClient, lbID, name string) (*pools.Pool, error) {
	pages, err := pools.List(c, pools.ListOpts{LoadbalancerID: lbID, Name: name}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	all, err := pools.ExtractPools(pages)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, service.ErrNotFound
	}
	return &all[0], nil
}

// listPools returns every pool of one load balancer.
func listPools(ctx context.Context, c *gophercloud.ServiceClient, lbID string) ([]pools.Pool, error) {
	pages, err := pools.List(c, pools.ListOpts{LoadbalancerID: lbID}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return pools.ExtractPools(pages)
}

// deletePool removes one pool and waits for the load balancer to settle.
func deletePool(ctx context.Context, c *gophercloud.ServiceClient, lbID, poolID string) error {
	if err := pools.Delete(ctx, c, poolID).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			return nil
		}
		return err
	}
	_, err := service.WaitActive(ctx, c, lbID)
	return err
}

// updateListener applies an update and waits for the load balancer to settle.
func updateListener(ctx context.Context, c *gophercloud.ServiceClient, lbID, listenerID string, opts listeners.UpdateOpts) error {
	if _, err := listeners.Update(ctx, c, listenerID, opts).Extract(); err != nil {
		return err
	}
	_, err := service.WaitActive(ctx, c, lbID)
	return err
}

// listL7Policies returns a listener's policies.
func listL7Policies(ctx context.Context, c *gophercloud.ServiceClient, listenerID string) ([]l7policies.L7Policy, error) {
	pages, err := l7policies.List(c, l7policies.ListOpts{ListenerID: listenerID}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return l7policies.ExtractL7Policies(pages)
}

// listL7Rules returns one policy's rules.
func listL7Rules(ctx context.Context, c *gophercloud.ServiceClient, policyID string) ([]l7policies.Rule, error) {
	pages, err := l7policies.ListRules(c, policyID, l7policies.ListRulesOpts{}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return l7policies.ExtractRules(pages)
}

// createL7Policy creates a policy and waits for the load balancer to settle.
func createL7Policy(ctx context.Context, c *gophercloud.ServiceClient, lbID string, opts l7policies.CreateOpts) (*l7policies.L7Policy, error) {
	pol, err := l7policies.Create(ctx, c, opts).Extract()
	if err != nil {
		return nil, err
	}
	if _, err := service.WaitActive(ctx, c, lbID); err != nil {
		return nil, err
	}
	return pol, nil
}

// createL7Rule adds one rule to a policy and waits for the load balancer.
func createL7Rule(ctx context.Context, c *gophercloud.ServiceClient, lbID, policyID string, opts l7policies.CreateRuleOpts) error {
	if _, err := l7policies.CreateRule(ctx, c, policyID, opts).Extract(); err != nil {
		return err
	}
	_, err := service.WaitActive(ctx, c, lbID)
	return err
}

// deleteL7Policy removes a policy (its rules go with it) and waits.
func deleteL7Policy(ctx context.Context, c *gophercloud.ServiceClient, lbID, policyID string) error {
	if err := l7policies.Delete(ctx, c, policyID).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			return nil
		}
		return err
	}
	_, err := service.WaitActive(ctx, c, lbID)
	return err
}

// notFound reports whether err is either kind of not-found this package meets.
func notFound(err error) bool {
	return err == service.ErrNotFound || gophercloud.ResponseCodeIs(err, 404)
}

var _ = fmt.Sprintf // keep fmt for the files that share this package
