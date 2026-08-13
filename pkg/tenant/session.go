// Package zun talks to the OpenStack Zun capsule API on behalf of one tenant.
package tenant

import (
	"context"
	"fmt"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
)

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
type Session struct {
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
func NewSession(ctx context.Context, creds Credentials) (*Session, error) {
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
	project, err := projectOf(pc)
	if err != nil {
		return nil, err
	}
	return &Session{provider: pc, region: creds.Region, project: project}, nil
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

// Provider is the authenticated session, for building clients for the other
// OpenStack services this tenant uses.
func (c *Session) Provider() *gophercloud.ProviderClient { return c.provider }

// Region the tenant's endpoints are resolved in.
func (c *Session) Region() string { return c.region }

// Project is the OpenStack project this client authenticated as. Empty when it
// could not be determined, which the binding check reads as unknown rather
// than as a mismatch.
func (c *Session) Project() string { return c.project }

// NewClientForTest builds a client that reports the given binding and can do
// nothing else. The binding check is the reason: it compares what a credential
// authenticated as against what was recorded, and a test of that comparison
// needs to state both sides without a Keystone.
func NewSessionForTest(project, region string) *Session {
	return &Session{project: project, region: region}
}
