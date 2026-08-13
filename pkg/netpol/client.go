package netpol

import (
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/fivetime/kubezun/pkg/tenant"
)

// NewClient builds the network client this package works through, from the
// same tenant session everything else here uses. There is no admin credential
// anywhere in this path: a tenant's security groups belong to the tenant.
func NewClient(client *tenant.Session) (*gophercloud.ServiceClient, error) {
	if client == nil || client.Provider() == nil {
		return nil, fmt.Errorf("an authenticated OpenStack session is required")
	}
	return openstack.NewNetworkV2(client.Provider(),
		gophercloud.EndpointOpts{Region: client.Region()})
}
