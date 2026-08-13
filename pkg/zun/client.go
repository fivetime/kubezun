// Package zun talks to the OpenStack Zun capsule API on behalf of one tenant.
package zun

import (
	"context"
	"fmt"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
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
	// provider is the authenticated session behind it. Other OpenStack
	// services this tenant talks to — Octavia for its Services — are built
	// from the same one, so a tenant authenticates once and holds one token.
	provider *gophercloud.ProviderClient
	region   string
	// project is which OpenStack project the credential authenticated as, read
	// from the token rather than from configuration. See projectOf.
	project string
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
	// gophercloud registers this endpoint as "application-container" and
	// builds the microversion header from that name, but Zun only accepts
	// "OpenStack-API-Version: container <v>" and answers 406 to anything else.
	sc.Type = "container"
	sc.Microversion = Microversion

	project, err := projectOf(pc)
	if err != nil {
		return nil, err
	}
	return &Client{sc: sc, provider: pc, region: creds.Region, project: project}, nil
}

// projectOf reads which OpenStack project a credential authenticated as.
//
// ⚠️ Read here rather than taken from configuration, because the point of
// having it is to catch configuration that disagrees with reality. A
// credential swapped for one in another project is otherwise silent: every
// call succeeds, against the wrong project.
func projectOf(pc *gophercloud.ProviderClient) (string, error) {
	result, ok := pc.GetAuthResult().(tokens.CreateResult)
	if !ok {
		// Identity v2, or a session built some other way. Nothing to compare
		// against, and refusing here would break a deployment that never had
		// this check. The binding check treats an empty project as unknown.
		return "", nil
	}
	project, err := result.ExtractProject()
	if err != nil {
		return "", fmt.Errorf("reading the project this credential authenticates as: %w", err)
	}
	if project == nil {
		// An unscoped token. It can list nothing and create nothing, so this
		// is worth failing on rather than discovering one call later.
		return "", fmt.Errorf("this credential is not scoped to a project")
	}
	return project.ID, nil
}

// NewClientAt wraps an already-built service client, for a caller that holds a
// session of its own and for tests that need the capsule calls pointed at a
// server they control. Whether a code path talks to Zun at all is otherwise
// unobservable, and "it did nothing because it was guarded" and "it did nothing
// because it silently failed" leave the same trace.
func NewClientAt(sc *gophercloud.ServiceClient) *Client {
	return &Client{sc: sc, provider: sc.ProviderClient}
}

// Provider is the authenticated session, for building clients for the other
// OpenStack services this tenant uses.
func (c *Client) Provider() *gophercloud.ProviderClient { return c.provider }

// Region the tenant's endpoints are resolved in.
func (c *Client) Region() string { return c.region }

// Project is the OpenStack project this client authenticated as. Empty when it
// could not be determined, which the binding check reads as unknown rather
// than as a mismatch.
func (c *Client) Project() string { return c.project }

// ServiceClient exposes the underlying client for the capsule calls.
func (c *Client) ServiceClient() *gophercloud.ServiceClient { return c.sc }

// NewClientForTest builds a client that reports the given binding and can do
// nothing else. The binding check is the reason: it compares what a credential
// authenticated as against what was recorded, and a test of that comparison
// needs to state both sides without a Keystone.
func NewClientForTest(project, region string) *Client {
	return &Client{project: project, region: region}
}
