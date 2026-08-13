package volume

import (
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/fivetime/kubezun/pkg/tenant"
)

// NewBlockStorageClient builds the Cinder client, on the same session and the
// same tenant credential as everything else here.
func NewBlockStorageClient(client *tenant.Session) (*gophercloud.ServiceClient, error) {
	if client == nil || client.Provider() == nil {
		return nil, fmt.Errorf("an authenticated OpenStack session is required")
	}
	return openstack.NewBlockStorageV3(client.Provider(),
		gophercloud.EndpointOpts{Region: client.Region()})
}

// NewSharedFSClient builds the Manila client. A deployment without Manila
// still serves ReadWriteOnce claims; only ReadWriteMany needs this.
func NewSharedFSClient(client *tenant.Session) (*gophercloud.ServiceClient, error) {
	if client == nil || client.Provider() == nil {
		return nil, fmt.Errorf("an authenticated OpenStack session is required")
	}
	sc, err := openstack.NewSharedFileSystemV2(client.Provider(),
		gophercloud.EndpointOpts{Region: client.Region()})
	if err != nil {
		return nil, err
	}
	// Export locations and access rules need a microversion later than the
	// 2.0 gophercloud defaults to; 2.14 covers both and every deployment
	// still running is newer than it.
	sc.Microversion = "2.14"
	return sc, nil
}
