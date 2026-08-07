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
	r := c.reconciler

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
	// A namespace cannot contain an underscore and a Service name cannot
	// either, so one split recovers both.
	namespace, service, found := strings.Cut(rest, "_")
	if !found || namespace == "" || service == "" {
		return "", "", false
	}
	return namespace, service, true
}
