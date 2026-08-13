package zun

import (
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/fivetime/kubezun/pkg/tenant"
)

// Microversion carrying the capsule fields this provider relies on.
const Microversion = "1.40"

// NewServiceClient builds the Zun endpoint of one tenant's session.
//
// Built here rather than inside the session for the same reason Neutron,
// Octavia and Cinder are: a session is an authenticated identity in a region,
// and every service is one endpoint resolved from it. Keeping Zun's inside the
// session made it the one service with a privileged position, which read as if
// a session were a Zun thing -- it is not, and the resolver that now hands them
// out serves every service alike.
func NewServiceClient(s *tenant.Session) (*gophercloud.ServiceClient, error) {
	sc, err := openstack.NewContainerV1(s.Provider(),
		gophercloud.EndpointOpts{Region: s.Region()})
	if err != nil {
		return nil, fmt.Errorf("find the container (zun) endpoint: %w", err)
	}
	// gophercloud registers this endpoint as "application-container" and builds
	// the microversion header from that name, but Zun only accepts
	// "OpenStack-API-Version: container <v>" and answers 406 to anything else.
	sc.Type = "container"
	sc.Microversion = Microversion
	return sc, nil
}
