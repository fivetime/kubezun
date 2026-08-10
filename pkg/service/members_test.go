package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeSubnets struct {
	byPod map[string]string
	err   error
}

func (f fakeSubnets) SubnetFor(_ context.Context, _, pod string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	s, ok := f.byPod[pod]
	if !ok {
		return "", fmt.Errorf("no subnet for %s", pod)
	}
	return s, nil
}

func slice(name string, family corev1.IPFamily, port int32, eps ...discoveryv1.Endpoint) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "t1-default", Name: name},
		AddressType: discoveryv1.AddressType(family),
		Ports:       []discoveryv1.EndpointPort{{Port: &port}},
		Endpoints:   eps,
	}
}

func endpoint(pod, addr string, ready bool) discoveryv1.Endpoint {
	r := ready
	return discoveryv1.Endpoint{
		Addresses:  []string{addr},
		Conditions: discoveryv1.EndpointConditions{Ready: &r},
		TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: pod},
	}
}

var port80 = corev1.ServicePort{Port: 80}

// The address that reaches Octavia is the pod's own, because a capsule's pod IP
// is its Neutron port address. Nothing translates it, and the subnet comes from
// the capsule rather than from the address.
func TestBuildMembersUsesThePodAddressAndItsSubnet(t *testing.T) {
	subnets := fakeSubnets{byPod: map[string]string{"web-1": "subnet-a", "web-2": "subnet-b"}}
	slices := []discoveryv1.EndpointSlice{slice("s1", corev1.IPv4Protocol, 8080,
		endpoint("web-1", "192.168.100.10", true),
		endpoint("web-2", "192.168.100.11", true),
	)}

	got, err := BuildMembers(context.Background(), subnets, slices, port80, corev1.IPv4Protocol)
	if err != nil {
		t.Fatalf("BuildMembers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d members, want 2: %+v", len(got), got)
	}
	if got[0].Address != "192.168.100.10" || *got[0].SubnetID != "subnet-a" {
		t.Errorf("member 0 = %s on %s, want the pod address on its own subnet",
			got[0].Address, *got[0].SubnetID)
	}
	// Pods on different subnets each keep their own; one shared subnet would
	// black-hole the other's traffic.
	if *got[1].SubnetID != "subnet-b" {
		t.Errorf("member 1 subnet = %s, want subnet-b", *got[1].SubnetID)
	}
	if got[0].ProtocolPort != 8080 {
		t.Errorf("port = %d, want the endpoint port rather than the service port", got[0].ProtocolPort)
	}
}

// Readiness is already folded into the endpoint condition, which is how a
// failing probe takes a capsule out of the pool without this code knowing what
// a probe is.
func TestBuildMembersSkipsUnreadyEndpoints(t *testing.T) {
	subnets := fakeSubnets{byPod: map[string]string{"ready": "subnet-a", "unready": "subnet-a"}}
	slices := []discoveryv1.EndpointSlice{slice("s1", corev1.IPv4Protocol, 80,
		endpoint("ready", "192.168.100.10", true),
		endpoint("unready", "192.168.100.11", false),
	)}

	got, err := BuildMembers(context.Background(), subnets, slices, port80, corev1.IPv4Protocol)
	if err != nil {
		t.Fatalf("BuildMembers: %v", err)
	}
	if len(got) != 1 || got[0].Address != "192.168.100.10" {
		t.Errorf("got %+v, want only the ready endpoint", got)
	}
}

// Octavia's batch update replaces the pool rather than adding to it. Returning
// the members that happened to resolve would take every other member out of
// service, so one unresolvable member fails the whole build.
func TestBuildMembersFailsRatherThanReturningAPartialSet(t *testing.T) {
	subnets := fakeSubnets{byPod: map[string]string{"web-1": "subnet-a"}} // web-2 missing
	slices := []discoveryv1.EndpointSlice{slice("s1", corev1.IPv4Protocol, 80,
		endpoint("web-1", "192.168.100.10", true),
		endpoint("web-2", "192.168.100.11", true),
	)}

	got, err := BuildMembers(context.Background(), subnets, slices, port80, corev1.IPv4Protocol)
	if err == nil {
		t.Fatalf("a member that could not be resolved produced %d members instead of an error", len(got))
	}
	if got != nil {
		t.Errorf("a failed build returned %d members; a caller passing those to a "+
			"full-set update would empty the pool", len(got))
	}
}

// An endpoint with no pod behind it cannot be given a subnet, and a member with
// the wrong subnet drops traffic silently, so it is refused rather than guessed.
func TestBuildMembersRefusesEndpointsWithNoPod(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{slice("s1", corev1.IPv4Protocol, 80,
		discoveryv1.Endpoint{Addresses: []string{"192.168.100.10"}},
	)}
	if _, err := BuildMembers(context.Background(), fakeSubnets{}, slices, port80, corev1.IPv4Protocol); err == nil {
		t.Error("an endpoint with no pod reference was accepted")
	}
}

// One pool serves one address family; mixing them would program a v6 member
// into a v4 pool.
func TestBuildMembersKeepsAddressFamiliesApart(t *testing.T) {
	subnets := fakeSubnets{byPod: map[string]string{"v4": "subnet-a", "v6": "subnet-b"}}
	slices := []discoveryv1.EndpointSlice{
		slice("s4", corev1.IPv4Protocol, 80, endpoint("v4", "192.168.100.10", true)),
		slice("s6", corev1.IPv6Protocol, 80, endpoint("v6", "fd00::10", true)),
	}

	got, err := BuildMembers(context.Background(), subnets, slices, port80, corev1.IPv4Protocol)
	if err != nil {
		t.Fatalf("BuildMembers: %v", err)
	}
	if len(got) != 1 || got[0].Address != "192.168.100.10" {
		t.Errorf("got %+v, want only the v4 member", got)
	}
}

// A slice that does not carry the Service's port yet contributes nothing rather
// than a member on port zero.
func TestBuildMembersIgnoresSlicesWithoutThePort(t *testing.T) {
	subnets := fakeSubnets{byPod: map[string]string{"web": "subnet-a"}}
	named := corev1.ServicePort{Name: "http", Port: 80}
	slices := []discoveryv1.EndpointSlice{slice("s1", corev1.IPv4Protocol, 8080,
		endpoint("web", "192.168.100.10", true))} // slice port is unnamed

	got, err := BuildMembers(context.Background(), subnets, slices, named, corev1.IPv4Protocol)
	if err != nil {
		t.Fatalf("BuildMembers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing until the slice carries the named port", got)
	}
}

// A batch update with nothing to change is still a write: Octavia moves the
// pool and its load balancer to PENDING_UPDATE, which is the state in which it
// refuses every other change. Reconciles are frequent, so an unchanged set has
// to be recognised or the load balancer is almost never writable.
func TestUnchangedMembersAreRecognised(t *testing.T) {
	subnet := "subnet-1"
	desired := []pools.BatchUpdateMemberOpts{
		{Address: "10.0.0.1", ProtocolPort: 80, SubnetID: &subnet},
		{Address: "10.0.0.2", ProtocolPort: 80, SubnetID: &subnet},
	}
	current := []pools.Member{
		// Order differs on purpose: Octavia does not promise one.
		{Address: "10.0.0.2", ProtocolPort: 80, SubnetID: subnet},
		{Address: "10.0.0.1", ProtocolPort: 80, SubnetID: subnet},
	}
	if !sameMembers(current, desired) {
		t.Error("an unchanged set was reported as changed; every reconcile would write")
	}

	for _, tc := range []struct {
		name    string
		current []pools.Member
	}{
		{"one fewer", current[:1]},
		{"different address", []pools.Member{
			{Address: "10.0.0.9", ProtocolPort: 80, SubnetID: subnet},
			{Address: "10.0.0.1", ProtocolPort: 80, SubnetID: subnet},
		}},
		{"different port", []pools.Member{
			{Address: "10.0.0.2", ProtocolPort: 8080, SubnetID: subnet},
			{Address: "10.0.0.1", ProtocolPort: 80, SubnetID: subnet},
		}},
		// The subnet is what lets a worker-based provider reach the member at
		// all, so a member that gained or lost one is not the same member.
		{"different subnet", []pools.Member{
			{Address: "10.0.0.2", ProtocolPort: 80, SubnetID: "subnet-2"},
			{Address: "10.0.0.1", ProtocolPort: 80, SubnetID: subnet},
		}},
	} {
		if sameMembers(tc.current, desired) {
			t.Errorf("%s: reported as unchanged; the pool would never be corrected", tc.name)
		}
	}

	// An empty pool that should stay empty must not be written either.
	if !sameMembers(nil, nil) {
		t.Error("two empty sets differ")
	}
}
