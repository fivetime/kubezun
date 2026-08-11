package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/config"

	"github.com/fivetime/kubezun/pkg/netpol"
)

func main() {
	ctx := context.Background()
	ao, _ := openstack.AuthOptionsFromEnv()
	ao.ApplicationCredentialID = os.Getenv("OS_APPLICATION_CREDENTIAL_ID")
	ao.ApplicationCredentialSecret = os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")
	ao.Username, ao.Password, ao.TenantName = "", "", ""
	pc, err := config.NewProviderClient(ctx, ao)
	if err != nil {
		panic(err)
	}
	nc, err := openstack.NewNetworkV2(pc, gophercloud.EndpointOpts{Region: os.Getenv("OS_REGION_NAME")})
	if err != nil {
		panic(err)
	}
	n := &netpol.Neutron{Client: nc}
	in, eg, err := n.EnsureBaseline(ctx)
	if err != nil {
		fmt.Println("FAILED", err)
		return
	}
	fmt.Println("baseline:", in, eg)
}
