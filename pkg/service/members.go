// Package service turns a tenant's Kubernetes Services into Octavia load
// balancers on their own network.
//
// Adapted from kubetron's reconciler of the same shape (Apache 2.0,
// github.com/fivetime/kubetron, pkg/service), which resolves a member's subnet
// from the NetworkPortClaim its pods carry. A capsule has no claim — Zun
// creates its port inline — so that resolution is the one part rewritten here;
// everything a member needs is on the capsule itself.
package service

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

// SubnetResolver reports which Neutron subnet a pod's address lives on.
//
// Octavia binds a member to a subnet, and a member bound to the wrong one
// black-holes traffic silently, so there is deliberately no empty-subnet
// fallback anywhere below: a pod whose subnet cannot be established is an
// error, not a member with a blank field.
type SubnetResolver interface {
	SubnetFor(ctx context.Context, namespace, podName string) (subnetID string, err error)
}

// BuildMembers renders the complete desired member set for one Service port.
//
// The result is the whole set, always. Octavia's batch update is a full-set
// PUT: a partial list does not add to the pool, it replaces it, so returning
// members for only the endpoints that happened to resolve would take every
// other member out of service.
func BuildMembers(
	ctx context.Context,
	subnets SubnetResolver,
	slices []discoveryv1.EndpointSlice,
	svcPort corev1.ServicePort,
	family corev1.IPFamily,
) ([]pools.BatchUpdateMemberOpts, error) {

	var members []pools.BatchUpdateMemberOpts
	for i := range slices {
		s := &slices[i]
		if string(s.AddressType) != string(family) {
			// One pool serves one address family.
			continue
		}
		port := resolveSlicePort(s, svcPort)
		if port == 0 {
			// This slice does not carry the Service's port yet.
			continue
		}
		for _, ep := range s.Endpoints {
			// Readiness is already folded into this bit by the EndpointSlice
			// controller, which is how a failing probe takes a capsule out of
			// the pool without anything here knowing what a probe is.
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if len(ep.Addresses) == 0 {
				continue
			}
			if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" {
				return nil, fmt.Errorf(
					"endpoint %v has no pod to resolve a subnet from", ep.Addresses)
			}

			subnetID, err := subnets.SubnetFor(ctx, s.Namespace, ep.TargetRef.Name)
			if err != nil {
				return nil, fmt.Errorf("member %s: %w", ep.TargetRef.Name, err)
			}

			// The address is the pod's, which for a capsule is its Neutron port
			// address — the invariant the whole data plane rests on. Nothing
			// here translates it.
			addr := ep.Addresses[0]
			name := ep.TargetRef.Name
			sid := subnetID
			members = append(members, pools.BatchUpdateMemberOpts{
				Address:      addr,
				ProtocolPort: port,
				Name:         &name,
				SubnetID:     &sid,
			})
		}
	}
	return members, nil
}

// resolveSlicePort returns the endpoint port matching the Service port by name.
// An unnamed Service port matches an unnamed slice port, which is the single
// port case.
func resolveSlicePort(s *discoveryv1.EndpointSlice, svcPort corev1.ServicePort) int {
	for _, p := range s.Ports {
		if p.Port == nil {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		if name == svcPort.Name {
			return int(*p.Port)
		}
	}
	return 0
}
