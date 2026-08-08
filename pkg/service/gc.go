package service

import (
	"context"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// gcInterval is how often load balancers are reconciled against the Services
// that should own them. An orphan costs the tenant quota and, if it holds a
// public address, money — but nothing breaks while one exists, so this runs
// far less often than the reconcile.
const gcInterval = 5 * time.Minute

// RunGC removes load balancers whose Service is gone.
//
// They accumulate whenever a Service is deleted while this process is not
// running: the delete event is the only thing that would have removed one, and
// nothing else ever will. Deleting a load balancer still in use would take a
// tenant's Service off the air, so every check here fails closed — anything
// uncertain is left for the next sweep.
func (c *Controller) RunGC(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

func (c *Controller) sweep(ctx context.Context) {
	r := c.reconciler

	// ⚠️ An empty served set is not "serves nothing", it is "does not know
	// yet" — the watch behind it fails closed. Sweeping on that would read
	// every one of the tenant's load balancers as belonging to a namespace it
	// does not serve and delete all of them.
	if r.Namespaces == nil || len(r.Namespaces()) == 0 {
		log.G(ctx).Warn("load balancer sweep skipped: no namespaces are known yet")
		return
	}

	all, err := listLoadBalancers(ctx, r.Octavia)
	if err != nil {
		log.G(ctx).WithError(err).Warn("load balancer sweep skipped: could not list them")
		return
	}

	for _, lb := range all {
		namespace, name, ok := r.parseLBName(lb.Name)
		if !ok {
			// Someone else's, or this tenant's from another deployment. Not
			// ours to judge.
			continue
		}

		// A load balancer for a namespace this process does not serve is one it
		// should never have built. They exist because the Service informer
		// spans the cluster and the reconciler did not check, so this is both
		// the cleanup for that and what keeps it from lingering if the served
		// set shrinks.
		if !c.serves(namespace) {
			log.G(ctx).WithField("service", namespace+"/"+name).
				WithField("loadbalancer", lb.ID).
				Info("deleting a load balancer for a namespace this process does not serve")
			if err := r.tearDown(ctx, namespace, name, lb.ID); err != nil {
				log.G(ctx).WithError(err).WithField("loadbalancer", lb.ID).
					Warn("could not delete it")
			}
			continue
		}

		if _, err := r.Services.Services(namespace).Get(name); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			// The cache could not answer. Treating that as "the Service is
			// gone" would delete live load balancers during an outage.
			log.G(ctx).WithError(err).WithField("service", namespace+"/"+name).
				Warn("load balancer sweep skipped one: the Service lookup failed")
			continue
		}

		log.G(ctx).WithField("service", namespace+"/"+name).
			WithField("loadbalancer", lb.ID).Info("deleting orphaned load balancer")
		if err := r.tearDown(ctx, namespace, name, lb.ID); err != nil {
			log.G(ctx).WithError(err).WithField("loadbalancer", lb.ID).
				Warn("could not delete an orphaned load balancer")
		}
	}

	c.sweepAddressPorts(ctx, all)
}

// sweepAddressPorts removes address ports whose load balancer is gone.
//
// They are left behind when this process dies between deleting a load balancer
// and releasing its port — the port is deleted second on purpose, since Neutron
// refuses while the address is in use. One costs an address on the tenant's
// subnet and nothing reports it, because the Service it belonged to is gone
// too.
func (c *Controller) sweepAddressPorts(ctx context.Context, all []loadbalancers.LoadBalancer) {
	r := c.reconciler
	if r.Neutron == nil {
		return
	}

	live := make(map[string]struct{}, len(all))
	for _, lb := range all {
		live[vipPortName(lb.Name)] = struct{}{}
	}

	prefix := "kubezun_" + r.Tenant + "_"
	pages, err := ports.List(r.Neutron, ports.ListOpts{}).AllPages(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("address port sweep skipped: could not list ports")
		return
	}
	found, err := ports.ExtractPorts(pages)
	if err != nil {
		log.G(ctx).WithError(err).Warn("address port sweep skipped: could not read ports")
		return
	}

	for _, p := range found {
		// Only this tenant's, and only the ones this package names. Anything
		// else on the subnet belongs to somebody else.
		if !strings.HasPrefix(p.Name, prefix) || !strings.HasSuffix(p.Name, "_vip") {
			continue
		}
		if _, ok := live[p.Name]; ok {
			continue
		}
		log.G(ctx).WithField("port", p.Name).
			Info("releasing an address port whose load balancer is gone")
		if err := ports.Delete(ctx, r.Neutron, p.ID).ExtractErr(); err != nil {
			log.G(ctx).WithError(err).WithField("port", p.Name).
				Warn("could not release it")
		}
	}
}

// parseLBName recovers the Service a load balancer belongs to, and reports
// whether the name is one of this tenant's at all.
//
// The prefix carries the tenant, so a sweep never considers another tenant's
// load balancers — nor kubetron's, whose names begin differently.
func (r *Reconciler) parseLBName(name string) (namespace, service string, ok bool) {
	prefix := "kubezun_" + r.Tenant + "_"
	if !strings.HasPrefix(name, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	// A namespace cannot contain an underscore and a Service name cannot
	// either, so one split recovers both.
	namespace, service, found := strings.Cut(rest, "_")
	if !found || namespace == "" || service == "" {
		return "", "", false
	}
	return namespace, service, true
}
