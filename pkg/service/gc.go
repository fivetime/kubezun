package service

import (
	"context"
	"strings"
	"time"

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
	_ = c.eachReconciler(ctx, func(r *Reconciler) error {
		c.sweepOne(ctx, r)
		return nil
	})
}

func (c *Controller) sweepOne(ctx context.Context, r *Reconciler) {
	// ⚠️ An empty served set is not "serves nothing", it is "does not know
	// yet" — the watch behind it fails closed. Sweeping on that would read
	// every one of the tenant's load balancers as belonging to a namespace it
	// does not serve and delete all of them.
	if r.Namespaces == nil || len(r.Namespaces()) == 0 {
		log.G(ctx).Warn("load balancer sweep skipped: no namespaces are known yet")
		return
	}

	all, err := ListLoadBalancers(ctx, r.Octavia)
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
	// ⚠️ Ingress load balancers share the tenant prefix with an extra "ing"
	// segment ("kubezun_<tenant>_ing_<ns>_<name>"). Splitting on the first
	// underscore would read "ing" as a namespace here, find no such Service,
	// and this sweep would delete every Ingress load balancer as an orphan.
	// Segment count is the discriminator: a namespace cannot contain an
	// underscore, so a Service name splits into exactly two parts.
	if strings.HasPrefix(rest, "ing_") {
		return "", "", false
	}
	// A namespace cannot contain an underscore and a Service name cannot
	// either, so one split recovers both.
	namespace, service, found := strings.Cut(rest, "_")
	if !found || namespace == "" || service == "" {
		return "", "", false
	}
	return namespace, service, true
}
