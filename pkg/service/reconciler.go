package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/record"
)

const (
	// ovnProvider is the amphora-less driver: a load balancer becomes OVN
	// northbound rules that ovn-controller applies on every hypervisor, so
	// there is no appliance per Service to pay for or to fail.
	ovnProvider = "ovn"

	// lbIDAnnotation remembers which load balancer belongs to a Service, so a
	// rename or a lookup failure cannot orphan one.
	lbIDAnnotation = "knaas.io/loadbalancer-id"

	// AddressAnnotation carries the address a Service is actually reachable on.
	//
	// The gateway reads it and reports it as the Service's cluster address, so
	// a tenant sees, and their resolver answers, the address that works — the
	// one the API server assigned is from a range that does not exist on their
	// network. The key is the gateway's rather than ours because nothing about
	// it should assume a load balancer: another data plane would fill the same
	// key.
	//
	// The status field would be the natural place and cannot be used: the API
	// server allows it only on a Service typed LoadBalancer, and here every
	// Service has an address.
	AddressAnnotation = "kubezoo.io/cluster-ip"
)

// Reconciler turns a tenant's Services into Octavia load balancers.
type Reconciler struct {
	// Octavia is the tenant's own client, so every load balancer it creates
	// belongs to the tenant's project and counts against their quota.
	Octavia *gophercloud.ServiceClient
	// Subnets resolves a member's subnet from its capsule.
	Subnets SubnetResolver

	// ServesNamespace reports whether a Service is this tenant's to act on.
	//
	// ⚠️ Not a filter for tidiness. The Service informer spans the cluster —
	// it has to, because a tenant's namespaces are not known until they are
	// watched — so without this the reconciler builds a load balancer, in this
	// tenant's OpenStack project and against this tenant's quota, for every
	// Service in the cluster: other tenants' and the platform's own. Measured:
	// 19 of them, including another tenant's kube-dns.
	ServesNamespace func(namespace string) bool

	// servedNamespaces lists them, so the sweep can tell "serves nothing" from
	// "does not know yet" and refuse to run on the second.
	Namespaces func() []string

	Services      corev1listers.ServiceLister
	Slices        discoveryv1listers.EndpointSliceLister
	ServiceClient corev1client.ServicesGetter

	// VIPSubnetID is the subnet load balancer addresses come from. It is not
	// the pod subnet: an address on the pod subnet makes east-west traffic
	// arrive with the wrong destination MAC.
	VIPSubnetID string
	// VIPNetworkID is the network that subnet belongs to, needed to create the
	// address port.
	VIPNetworkID string

	// Tenant scopes the names of the objects created, so several tenants
	// sharing one OpenStack can tell theirs apart, and so a garbage collector
	// can recognise its own.
	Tenant string

	// Events records why a Service has no working address yet. Without one the
	// gateway reports the address the API server assigned, which does not work,
	// and a tenant has nowhere to look for the reason.
	Events record.EventRecorder

	// Neutron allocates the public addresses. Nil means none can be given.
	Neutron *gophercloud.ServiceClient
	// FloatingNetworkID is the external network public addresses come from.
	FloatingNetworkID string
	// PublicByDefault decides what a LoadBalancer Service gets when it says
	// nothing. False keeps a tenant off the public network until they ask,
	// because a public address costs the platform money and exposes a service
	// its author may have only meant to reach from inside.
	PublicByDefault bool
}

// Name of the load balancer backing a Service. Derived rather than random so a
// load balancer can still be found after its annotation is lost.
func (r *Reconciler) lbName(namespace, name string) string {
	return fmt.Sprintf("kubezun_%s_%s_%s", r.Tenant, namespace, name)
}

// Reconcile brings one Service's load balancer into line with its endpoints.
func (r *Reconciler) Reconcile(ctx context.Context, namespace, name string) error {
	svc, err := r.Services.Services(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		// The Service is gone. Its load balancer is found by the name derived
		// from the namespace and name, which is why that name is derived and
		// not random: there is no object left to read an id off.
		return r.deleteByName(ctx, namespace, name)
	}
	if err != nil {
		return err
	}
	// Every Service gets a load balancer, not only the ones typed
	// LoadBalancer. A capsule has one interface, on the tenant's network, so
	// the cluster's own ClusterIP is unreachable from it — measured: a capsule
	// reaches a peer's pod address and cannot reach the Service's ClusterIP at
	// all. A pod on the cluster CNI can be given a second interface to bridge
	// that; a capsule cannot. So the load balancer is not an extra for
	// externally exposed Services, it is how a Service works here.
	//
	// The provider makes that affordable: it is amphora-less, so a load
	// balancer is northbound rules rather than an appliance per Service.
	//
	// A headless Service asks for no address at all and gets none.
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return nil
	}
	if svc.DeletionTimestamp != nil {
		return r.deleteLoadBalancer(ctx, svc)
	}

	lb, err := r.ensureLoadBalancer(ctx, svc)
	if err != nil {
		return err
	}
	portID := lb.VipPortID

	slices, err := r.slicesFor(svc)
	if err != nil {
		return err
	}
	for _, port := range svc.Spec.Ports {
		if err := r.ensurePort(ctx, svc, lb, port, slices); err != nil {
			return err
		}
	}

	address := lb.VipAddress
	if wantsPublicAddress(svc, r.PublicByDefault) {
		network := r.FloatingNetworkID
		if v := svc.Annotations[FloatingNetworkAnnotation]; v != "" {
			network = v
		}
		public, err := ensureFloatingIP(ctx, r.Neutron, portID, network)
		if err != nil {
			return err
		}
		address = public
	} else {
		// Asked for one before and does not now: give it back rather than
		// leave it allocated and charged for.
		if err := releaseFloatingIP(ctx, r.Neutron, portID); err != nil {
			return err
		}
	}
	// No name is published for the address. The tenant runs its own resolver,
	// which answers from the Service objects it reads through the gateway, and
	// the address it answers with is the one publishAddress puts on the Service
	// below — so a name published here would be a second copy of the same fact,
	// kept in a different system, that no tenant application ever asks for.
	//
	// It was tried: the address port carried a dns_name and Neutron published
	// it. What that produced was <svc>.<tid>-<namespace>.svc.cluster.local,
	// the name the platform uses. A tenant's application asks for
	// <svc>.<namespace>.svc.cluster.local, because that is the namespace it can
	// see — and no global DNS namespace can serve that name, since every tenant
	// needs it to resolve to a different address.
	return r.publishAddress(ctx, svc, address)
}

func (r *Reconciler) ensureLoadBalancer(ctx context.Context, svc *corev1.Service) (*loadbalancers.LoadBalancer, error) {
	// By recorded id first: a Service renamed or recreated still points at the
	// load balancer it already has, and creating a second one would leave the
	// first running and charged for.
	if id := svc.Annotations[lbIDAnnotation]; id != "" {
		lb, err := GetLoadBalancerByID(ctx, r.Octavia, id)
		if err == nil {
			// Returned as it is, never replaced. A load balancer this process
			// finds by id is the tenant's address, and Kubernetes does not
			// change a Service's address while it exists — so there is no
			// condition observable from here that justifies building another
			// one and moving the tenant onto it.
			//
			// ⚠️ There was such a condition here: the address port was read
			// from Neutron and a 404 taken to mean the load balancer had been
			// left broken by a failed create. It is not evidence of that. All
			// three of the lab's load balancers are ACTIVE, operating ONLINE
			// and carrying members, and the vip_port_id each one reports does
			// not resolve even for an administrator — the port is there
			// shortly after creation and gone later, while the load balancer
			// goes on working, because the OVN provider carries the address in
			// the data plane rather than on a port that has to persist.
			//
			// So the check was true on every healthy load balancer, and every
			// reconcile deleted all three and built new ones, handing the
			// tenant a different address each time. The log said the load
			// balancer had no address port while this process was the only
			// thing breaking anything.
			return WaitActive(ctx, r.Octavia, lb.ID)
		} else if err != ErrNotFound {
			return nil, fmt.Errorf("reading load balancer %s: %w", id, err)
		}
		// Deleted behind our back; fall through and make another.
	}

	name := r.lbName(svc.Namespace, svc.Name)
	lb, err := GetLoadBalancerByName(ctx, r.Octavia, name)
	switch {
	case err == nil:
	case err == ErrNotFound:
		lb, err = loadbalancers.Create(ctx, r.Octavia, loadbalancers.CreateOpts{
			Name:        name,
			Description: fmt.Sprintf("kubezun Service %s/%s", svc.Namespace, svc.Name),
			VipSubnetID: r.VIPSubnetID,
			Provider:    ovnProvider,
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("creating load balancer %q on subnet %s: %w",
				name, r.VIPSubnetID, err)
		}
		// Published before waiting for the load balancer to finish
		// provisioning. The address is settled the moment the create returns —
		// a few seconds — while provisioning takes tens of them, and until it
		// is published the gateway reports the address the API server
		// assigned, which nothing on the tenant's network can reach. An
		// application that resolves the name in that window may cache what it
		// got for far longer than the window lasts.
		if err := r.publishAddress(ctx, svc, lb.VipAddress); err != nil {
			log.G(ctx).WithError(err).WithField("service", svc.Namespace+"/"+svc.Name).
				Warn("could not publish the address yet; the gateway still reports the unreachable one")
		}
	default:
		return nil, fmt.Errorf("looking up load balancer %q: %w", name, err)
	}

	lb, err = WaitActive(ctx, r.Octavia, lb.ID)
	if err != nil {
		return nil, err
	}
	if err := r.recordLoadBalancerID(ctx, svc, lb.ID); err != nil {
		return nil, err
	}
	return lb, nil
}

// recordLoadBalancerID writes the id onto the Service. Done immediately after
// creation: if this process dies before it, the next pass finds the load
// balancer by name, and the annotation only has to survive a rename.
func (r *Reconciler) recordLoadBalancerID(ctx context.Context, svc *corev1.Service, id string) error {
	if svc.Annotations[lbIDAnnotation] == id {
		return nil
	}
	updated := svc.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[lbIDAnnotation] = id
	_, err := r.ServiceClient.Services(svc.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func (r *Reconciler) ensurePort(
	ctx context.Context,
	svc *corev1.Service,
	lb *loadbalancers.LoadBalancer,
	sp corev1.ServicePort,
	slices []discoveryv1.EndpointSlice,
) error {
	proto := strings.ToUpper(string(sp.Protocol))
	if proto == "" {
		proto = "TCP"
	}
	if proto != "TCP" && proto != "UDP" && proto != "SCTP" {
		return fmt.Errorf("port %q: the OVN provider does not carry %s", sp.Name, proto)
	}

	name := fmt.Sprintf("%s_%s_%d", r.lbName(svc.Namespace, svc.Name), strings.ToLower(proto), sp.Port)

	listener, err := GetListenerByName(ctx, r.Octavia, lb.ID, name)
	switch {
	case err == nil:
	case err == ErrNotFound:
		listener, err = CreateListener(ctx, r.Octavia, lb.ID, listeners.CreateOpts{
			Name:           name,
			Protocol:       listeners.Protocol(proto),
			ProtocolPort:   int(sp.Port),
			LoadbalancerID: lb.ID,
		})
		if err != nil {
			return fmt.Errorf("creating listener %q: %w", name, err)
		}
	default:
		return fmt.Errorf("looking up listener %q: %w", name, err)
	}

	pool, err := GetPoolByListener(ctx, r.Octavia, lb.ID, listener.ID)
	switch {
	case err == nil:
	case err == ErrNotFound:
		pool, err = CreatePool(ctx, r.Octavia, lb.ID, pools.CreateOpts{
			Name:     name,
			Protocol: pools.Protocol(proto),
			// The only algorithm the OVN provider accepts; anything else is
			// refused at creation.
			LBMethod:   pools.LBMethodSourceIpPort,
			ListenerID: listener.ID,
		})
		if err != nil {
			return fmt.Errorf("creating pool %q: %w", name, err)
		}
	default:
		return fmt.Errorf("looking up the pool of listener %q: %w", name, err)
	}

	// A health monitor is the data plane's own check, separate from the pod's
	// readiness probe: readiness decides who is in the pool, this decides
	// whether a member in it is answering. The OVN provider carries only these
	// two types.
	if pool.MonitorID == "" {
		hmType := "TCP"
		if proto == "UDP" {
			hmType = "UDP-CONNECT"
		}
		if _, err := CreateHealthMonitor(ctx, r.Octavia, lb.ID, monitors.CreateOpts{
			Name:           name,
			PoolID:         pool.ID,
			Type:           hmType,
			Delay:          5,
			Timeout:        3,
			MaxRetries:     3,
			MaxRetriesDown: 3,
		}); err != nil {
			return fmt.Errorf("creating the health monitor of pool %q: %w", name, err)
		}
	}

	// Members have to match the address family of the load balancer's own
	// address, which the VIP subnet decided; a mismatched member is something
	// the provider cannot route to.
	family := corev1.IPv4Protocol
	if strings.Contains(lb.VipAddress, ":") {
		family = corev1.IPv6Protocol
	}
	members, err := BuildMembers(ctx, r.Subnets, slices, sp, family)
	if err != nil {
		return err
	}
	if err := SetPoolMembers(ctx, r.Octavia, lb.ID, pool.ID, members); err != nil {
		return fmt.Errorf("setting the %d members of pool %q: %w", len(members), name, err)
	}
	return nil
}

func (r *Reconciler) slicesFor(svc *corev1.Service) ([]discoveryv1.EndpointSlice, error) {
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	found, err := r.Slices.EndpointSlices(svc.Namespace).List(sel)
	if err != nil {
		return nil, err
	}
	out := make([]discoveryv1.EndpointSlice, 0, len(found))
	for _, s := range found {
		out = append(out, *s)
	}
	return out, nil
}

// publishAddress puts the load balancer's address in the Service status.
//
// This is the address that works, for every Service and not only the ones typed
// LoadBalancer: the ClusterIP the API server assigned is unreachable from a
// capsule, so a tenant reading it gets an address that goes nowhere. Publishing
// here is what lets them, and the tenant's DNS, find the one that does.
func (r *Reconciler) publishAddress(ctx context.Context, svc *corev1.Service, address string) error {
	if svc.Annotations[AddressAnnotation] != address {
		updated := svc.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[AddressAnnotation] = address
		fresh, err := r.ServiceClient.Services(svc.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		svc = fresh
	}

	// The status field is rejected on anything but a LoadBalancer Service, so a
	// ClusterIP one carries its address in the annotation alone. That is what
	// makes the tenant's DNS the way a Service is reached by name rather than a
	// convenience on top of one.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil
	}
	current := svc.Status.LoadBalancer.Ingress
	if len(current) == 1 && current[0].IP == address {
		return nil
	}
	updated := svc.DeepCopy()
	updated.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: address}}
	_, err := r.ServiceClient.Services(svc.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

// deleteByName removes the load balancer of a Service that no longer exists.
func (r *Reconciler) deleteByName(ctx context.Context, namespace, name string) error {
	lb, err := GetLoadBalancerByName(ctx, r.Octavia, r.lbName(namespace, name))
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return r.tearDown(ctx, namespace, name, lb.ID)
}

func (r *Reconciler) deleteLoadBalancer(ctx context.Context, svc *corev1.Service) error {
	id := svc.Annotations[lbIDAnnotation]
	if id == "" {
		lb, err := GetLoadBalancerByName(ctx, r.Octavia, r.lbName(svc.Namespace, svc.Name))
		if err == ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		id = lb.ID
	}
	return r.tearDown(ctx, svc.Namespace, svc.Name, id)
}

func (r *Reconciler) tearDown(ctx context.Context, namespace, name, id string) error {
	service := namespace + "/" + name
	// The public address goes back first, while it can still be found by the
	// port it is attached to. Deleting the load balancer takes that port with
	// it, and Neutron leaves the address allocated to the project — still
	// billed, attached to nothing, with nothing left to trace it from.
	if r.Neutron != nil {
		lb, err := GetLoadBalancerByID(ctx, r.Octavia, id)
		switch {
		case err == nil:
			if err := releaseFloatingIP(ctx, r.Neutron, lb.VipPortID); err != nil {
				return err
			}
		case err == ErrNotFound:
			// Already gone; any address it had is already orphaned and only a
			// sweep can find it now.
			log.G(ctx).WithField("loadbalancer", id).
				Warn("load balancer was already deleted; a public address it held cannot be traced from here")
		default:
			return err
		}
	}

	log.G(ctx).WithField("service", service).
		WithField("loadbalancer", id).Info("deleting load balancer")
	// Cascade takes the listeners, pools and the address port with it, and the
	// DNS record goes with the port.
	return DeleteLoadBalancer(ctx, r.Octavia, id)
}
