package service

import (
	"fmt"
	"os"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/fivetime/kubezun/pkg/zun"
)

// NewOctaviaClient builds the load balancer client for a tenant from the
// session it already holds for Zun, so a tenant authenticates once and carries
// one token rather than one per service.
func NewOctaviaClient(client *zun.Client) (*gophercloud.ServiceClient, error) {
	if client == nil || client.Provider() == nil {
		return nil, fmt.Errorf("an authenticated OpenStack session is required")
	}

	// The catalog publishes whatever URL the OpenStack deployment considers
	// internal, which is right when this runs beside OpenStack and useless when
	// it does not — several clusters sharing one Octavia get handed the owning
	// cluster's addresses. Same escape hatch kubetron needed for the same
	// reason.
	if ep := strings.TrimSpace(os.Getenv("OS_LOADBALANCER_ENDPOINT_OVERRIDE")); ep != "" {
		if !strings.HasSuffix(ep, "/") {
			ep += "/"
		}
		// ServiceURL builds request paths from ResourceBase, so the version
		// prefix has to be there exactly once however the operator wrote it.
		ep = strings.ReplaceAll(ep, "v2.0/", "")
		return &gophercloud.ServiceClient{
			ProviderClient: client.Provider(),
			Endpoint:       ep,
			ResourceBase:   ep + "v2.0/",
			Type:           "load-balancer",
		}, nil
	}

	return openstack.NewLoadBalancerV2(client.Provider(),
		gophercloud.EndpointOpts{Region: client.Region()})
}

// NewNetworkClient builds the Neutron client, from the same session, for
// allocating the public addresses a Service asks for.
func NewNetworkClient(client *zun.Client) (*gophercloud.ServiceClient, error) {
	if client == nil || client.Provider() == nil {
		return nil, fmt.Errorf("an authenticated OpenStack session is required")
	}
	if ep := strings.TrimSpace(os.Getenv("OS_NETWORK_ENDPOINT_OVERRIDE")); ep != "" {
		if !strings.HasSuffix(ep, "/") {
			ep += "/"
		}
		return &gophercloud.ServiceClient{
			ProviderClient: client.Provider(),
			Endpoint:       ep,
			ResourceBase:   ep + "v2.0/",
			Type:           "network",
		}, nil
	}
	return openstack.NewNetworkV2(client.Provider(),
		gophercloud.EndpointOpts{Region: client.Region()})
}
