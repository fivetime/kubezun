package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
)

// ErrNotFound is returned by the lookups below when nothing matched. It is a
// distinct error because "no load balancer yet" is the ordinary first pass of a
// reconcile, not a failure.
var ErrNotFound = errors.New("not found")

const (
	// activeTimeout bounds waiting for a load balancer to settle. Octavia
	// refuses to change one that is still provisioning, so every step waits;
	// a wait with no bound would hold a worker forever on a load balancer
	// stuck in ERROR.
	activeTimeout = 5 * time.Minute
	activePoll    = 2 * time.Second
)

// GetLoadBalancerByID reads one load balancer.
func GetLoadBalancerByID(ctx context.Context, c *gophercloud.ServiceClient, id string) (*loadbalancers.LoadBalancer, error) {
	lb, err := loadbalancers.Get(ctx, c, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return lb, nil
}

// GetLoadBalancerByName finds a load balancer by its exact name.
func GetLoadBalancerByName(ctx context.Context, c *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	pages, err := loadbalancers.List(c, loadbalancers.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	all, err := loadbalancers.ExtractLoadBalancers(pages)
	if err != nil {
		return nil, err
	}
	switch len(all) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &all[0], nil
	default:
		// Two load balancers under one name means something else is creating
		// them; picking one would leave the other running and billed.
		return nil, fmt.Errorf("%d load balancers are named %q", len(all), name)
	}
}

// ListLoadBalancers returns every load balancer the credential can see.
func ListLoadBalancers(ctx context.Context, c *gophercloud.ServiceClient) ([]loadbalancers.LoadBalancer, error) {
	pages, err := loadbalancers.List(c, loadbalancers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return loadbalancers.ExtractLoadBalancers(pages)
}

// WaitActive blocks until the load balancer is ACTIVE and returns it.
func WaitActive(ctx context.Context, c *gophercloud.ServiceClient, id string) (*loadbalancers.LoadBalancer, error) {
	deadline := time.Now().Add(activeTimeout)
	for {
		lb, err := GetLoadBalancerByID(ctx, c, id)
		if err != nil {
			return nil, err
		}
		switch lb.ProvisioningStatus {
		case "ACTIVE":
			return lb, nil
		case "ERROR":
			// Reported rather than waited out: Octavia will not leave this
			// state on its own, and the load balancer has to be deleted.
			return nil, fmt.Errorf("load balancer %s is in ERROR", id)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("load balancer %s was still %s after %s",
				id, lb.ProvisioningStatus, activeTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(activePoll):
		}
	}
}

// GetListenerByName finds a listener of one load balancer by name.
func GetListenerByName(ctx context.Context, c *gophercloud.ServiceClient, lbID, name string) (*listeners.Listener, error) {
	pages, err := listeners.List(c, listeners.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	all, err := listeners.ExtractListeners(pages)
	if err != nil {
		return nil, err
	}
	for i := range all {
		for _, lb := range all[i].Loadbalancers {
			if lb.ID == lbID {
				return &all[i], nil
			}
		}
	}
	return nil, ErrNotFound
}

// GetPoolByListener finds the pool attached to a listener.
func GetPoolByListener(ctx context.Context, c *gophercloud.ServiceClient, lbID, listenerID string) (*pools.Pool, error) {
	pages, err := pools.List(c, pools.ListOpts{LoadbalancerID: lbID}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	all, err := pools.ExtractPools(pages)
	if err != nil {
		return nil, err
	}
	for i := range all {
		for _, l := range all[i].Listeners {
			if l.ID == listenerID {
				return &all[i], nil
			}
		}
	}
	return nil, ErrNotFound
}

// CreateListener creates a listener and waits for the load balancer to settle.
func CreateListener(ctx context.Context, c *gophercloud.ServiceClient, lbID string, opts listeners.CreateOpts) (*listeners.Listener, error) {
	l, err := listeners.Create(ctx, c, opts).Extract()
	if err != nil {
		return nil, err
	}
	if _, err := WaitActive(ctx, c, lbID); err != nil {
		return nil, err
	}
	return l, nil
}

// CreatePool creates a pool and waits for the load balancer to settle.
func CreatePool(ctx context.Context, c *gophercloud.ServiceClient, lbID string, opts pools.CreateOpts) (*pools.Pool, error) {
	p, err := pools.Create(ctx, c, opts).Extract()
	if err != nil {
		return nil, err
	}
	if _, err := WaitActive(ctx, c, lbID); err != nil {
		return nil, err
	}
	return p, nil
}

// CreateHealthMonitor adds a monitor to a pool and waits for the load balancer
// to settle.
func CreateHealthMonitor(ctx context.Context, c *gophercloud.ServiceClient, lbID string, opts monitors.CreateOpts) (*monitors.Monitor, error) {
	m, err := monitors.Create(ctx, c, opts).Extract()
	if err != nil {
		return nil, err
	}
	if _, err := WaitActive(ctx, c, lbID); err != nil {
		return nil, err
	}
	return m, nil
}

// SetPoolMembers replaces a pool's members with exactly the given set.
//
// The call is a full-set PUT, which is why callers must pass the complete
// desired list: anything left out is removed from the pool, not left alone.
func SetPoolMembers(ctx context.Context, c *gophercloud.ServiceClient, lbID, poolID string, members []pools.BatchUpdateMemberOpts) error {
	if err := pools.BatchUpdateMembers(ctx, c, poolID, members).ExtractErr(); err != nil {
		return err
	}
	_, err := WaitActive(ctx, c, lbID)
	return err
}

// DeleteLoadBalancer removes a load balancer and everything under it.
func DeleteLoadBalancer(ctx context.Context, c *gophercloud.ServiceClient, id string) error {
	err := loadbalancers.Delete(ctx, c, id, loadbalancers.DeleteOpts{Cascade: true}).ExtractErr()
	if err != nil && !gophercloud.ResponseCodeIs(err, 404) {
		return err
	}
	return nil
}
