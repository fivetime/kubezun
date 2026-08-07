// Package zun talks to the OpenStack Zun capsule API on behalf of one tenant.
package zun

import (
	"context"
	"fmt"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
)

// Microversion carrying the capsule fields this provider relies on.
const Microversion = "1.40"

// Credentials identify one tenant against Keystone. An application credential
// is the only supported form: it is scoped to the tenant's project and can be
// revoked and expired independently of any user, whereas an admin credential
// would let a bug in this provider reach into other tenants' capsules (Zun's
// admin context forces all_projects=True).
type Credentials struct {
	AuthURL               string
	ApplicationCredID     string
	ApplicationCredName   string
	ApplicationCredSecret string
	Username              string
	UserDomainName        string
	Region                string
}

// CredentialsFromEnv reads the standard OS_* variables. Deployments give each
// tenant's virtual-kubelet its own Secret, so the process only ever holds one
// tenant's credentials.
func CredentialsFromEnv() (Credentials, error) {
	c := Credentials{
		AuthURL:               os.Getenv("OS_AUTH_URL"),
		ApplicationCredID:     os.Getenv("OS_APPLICATION_CREDENTIAL_ID"),
		ApplicationCredName:   os.Getenv("OS_APPLICATION_CREDENTIAL_NAME"),
		ApplicationCredSecret: os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET"),
		Username:              os.Getenv("OS_USERNAME"),
		UserDomainName:        os.Getenv("OS_USER_DOMAIN_NAME"),
		Region:                os.Getenv("OS_REGION_NAME"),
	}
	if c.AuthURL == "" {
		return c, fmt.Errorf("OS_AUTH_URL is required")
	}
	if c.ApplicationCredSecret == "" {
		return c, fmt.Errorf("OS_APPLICATION_CREDENTIAL_SECRET is required: " +
			"this provider authenticates with an application credential, not a password")
	}
	if c.ApplicationCredID == "" && c.ApplicationCredName == "" {
		return c, fmt.Errorf("either OS_APPLICATION_CREDENTIAL_ID or " +
			"OS_APPLICATION_CREDENTIAL_NAME is required")
	}
	if c.Region == "" {
		c.Region = "RegionOne"
	}
	return c, nil
}

// Client is a Zun service client bound to a single tenant.
type Client struct {
	sc *gophercloud.ServiceClient
}

// NewClient authenticates and returns a client for the capsule API.
func NewClient(ctx context.Context, creds Credentials) (*Client, error) {
	ao := gophercloud.AuthOptions{
		IdentityEndpoint:            creds.AuthURL,
		ApplicationCredentialID:     creds.ApplicationCredID,
		ApplicationCredentialName:   creds.ApplicationCredName,
		ApplicationCredentialSecret: creds.ApplicationCredSecret,
		Username:                    creds.Username,
		AllowReauth:                 true,
	}
	if creds.UserDomainName != "" {
		ao.DomainName = creds.UserDomainName
	}

	pc, err := openstack.AuthenticatedClient(ctx, ao)
	if err != nil {
		return nil, fmt.Errorf("authenticate against %s: %w", creds.AuthURL, err)
	}
	sc, err := openstack.NewContainerV1(pc, gophercloud.EndpointOpts{Region: creds.Region})
	if err != nil {
		return nil, fmt.Errorf("find the container (zun) endpoint: %w", err)
	}
	sc.Microversion = Microversion
	return &Client{sc: sc}, nil
}

// ServiceClient exposes the underlying client for the capsule calls.
func (c *Client) ServiceClient() *gophercloud.ServiceClient { return c.sc }
