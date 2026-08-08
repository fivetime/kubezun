package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

// portDNS names a port for the tenant's DNS.
//
// Written as a raw update because gophercloud's DNS extension models only
// dns_name, and the per-port dns_domain this needs comes from a second Neutron
// extension. Per port rather than per network on purpose: a tenant has one
// network and may have several namespaces, and the domain is what carries the
// namespace.
type portDNS struct {
	Name   string
	Domain string
}

func (o portDNS) ToPortUpdateMap() (map[string]any, error) {
	return map[string]any{"port": map[string]any{
		"dns_name":   o.Name,
		"dns_domain": o.Domain,
	}}, nil
}

// ServiceDomain is the DNS domain a namespace's Services live in, which is the
// one Kubernetes clients already look for.
func ServiceDomain(namespace, clusterDomain string) string {
	base := strings.TrimSuffix(clusterDomain, ".")
	if base == "" {
		base = "svc.cluster.local"
	}
	return fmt.Sprintf("%s.%s.", namespace, base)
}

// ensurePortDNS gives a port its name, which is what makes Neutron publish a
// record for the address and withdraw it when the port goes.
//
// Publishing this way rather than writing records directly is deliberate: the
// record's life becomes the port's life, so there is no state left to leak if
// this process dies between creating one and cleaning it up.
func ensurePortDNS(ctx context.Context, neutron *gophercloud.ServiceClient, portID, name, domain string) error {
	if neutron == nil || portID == "" {
		return nil
	}

	var current struct {
		Port struct {
			DNSName   string `json:"dns_name"`
			DNSDomain string `json:"dns_domain"`
		} `json:"port"`
	}
	if _, err := neutron.Get(ctx, neutron.ServiceURL("ports", portID), &current, nil); err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			// ⚠️ Not evidence the port is gone. Neutron answers 404 for a port
			// in another project just as it does for one that does not exist,
			// and a load balancer's address port belongs to Octavia's project,
			// not the tenant's — so this is the ordinary answer for a healthy
			// one. It is reported rather than acted on: the caller only warns,
			// and the Service keeps working at its address without a name.
			return fmt.Errorf("port %s cannot be read with this credential, so %s.%s was not published: %w",
				portID, name, domain, err)
		}
		return fmt.Errorf("reading port %s: %w", portID, err)
	}
	if current.Port.DNSName == name && current.Port.DNSDomain == domain {
		return nil
	}

	if err := ports.Update(ctx, neutron, portID, portDNS{Name: name, Domain: domain}).Err; err != nil {
		return fmt.Errorf("naming port %s as %s.%s: %w", portID, name, domain, err)
	}
	return nil
}
