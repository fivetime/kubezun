package provider

import (
	"context"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/fivetime/kubezun/pkg/zun"
)

const (
	// orphanSweepInterval is how often capsules are reconciled against the
	// pods that should own them. Orphans cost the tenant quota but nothing
	// else, so this runs far less often than the status sync.
	orphanSweepInterval = 2 * time.Minute

	// orphanGrace is how old a capsule must be before a missing pod is taken
	// as evidence that it was orphaned. A capsule created seconds ago may
	// simply not have reached this process's pod cache yet, and deleting it
	// would kill a pod that is starting normally.
	orphanGrace = 5 * time.Minute
)

// sweepOrphans deletes capsules this provider created for pods that no longer
// exist. They accumulate whenever a pod is deleted while the provider is not
// running, and nothing else will ever clean them up: they hold the tenant's
// quota and their Neutron ports indefinitely.
//
// Deleting a capsule that is still in use would destroy a running workload, so
// every check here is written to fail closed — anything uncertain is left
// alone and picked up by a later sweep.
func (p *Provider) sweepOrphans(ctx context.Context) {
	if p.podLister == nil {
		// Without a pod cache there is nothing to compare against, and every
		// capsule would look orphaned.
		return
	}

	managed, err := p.capsules.ListManagedAll(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("orphan sweep skipped: could not list capsules")
		return
	}
	log.G(ctx).WithField("pods", len(managed)).Debug("orphan sweep running")

	for key, capsules := range managed {
		for _, capsule := range p.orphansAmong(ctx, key, capsules) {
			log.G(ctx).WithField("pod", key).WithField("capsule", capsule.Name()).
				Info("deleting orphaned capsule")
			if err := p.capsules.Delete(ctx, capsule.UUID); err != nil {
				log.G(ctx).WithError(err).WithField("capsule", capsule.Name()).
					Warn("could not delete orphaned capsule")
			}
		}
	}
}

// orphansAmong returns the capsules of one pod that nothing needs any more.
//
// Two cases produce them: the pod is gone entirely, and a retried create left
// duplicates behind, of which only the newest carries the state the pod status
// reflects. Anything uncertain is left alone for a later sweep — deleting a
// capsule that is still in use destroys a running workload, while keeping an
// orphan an extra two minutes costs only quota.
func (p *Provider) orphansAmong(ctx context.Context, key string, capsules []*zun.Capsule) []*zun.Capsule {
	namespace, name, err := zun.SplitPodKey(key)
	if err != nil {
		return nil
	}
	if err := p.authorize(namespace); err != nil {
		// A capsule labelled for a namespace this node does not serve is
		// somebody else's to clean up.
		return nil
	}

	podGone := false
	var podUID string
	pod, err := p.podLister.Pods(namespace).Get(name)
	switch {
	case err == nil:
		podUID = string(pod.UID)
	case apierrors.IsNotFound(err):
		podGone = true
	default:
		// The cache could not answer; treating that as "pod is gone" would
		// delete live workloads during an API outage.
		log.G(ctx).WithError(err).WithField("pod", key).
			Warn("orphan sweep skipped a pod: lookup failed")
		return nil
	}

	// Among the capsules that do belong to the live pod, the newest is the one
	// kept; the rest are duplicates from retried creates.
	var keep *zun.Capsule
	if !podGone {
		for _, c := range capsules {
			if c.PodUID() != "" && c.PodUID() != podUID {
				continue
			}
			if keep == nil || c.CreatedAt.After(keep.CreatedAt.Time) {
				keep = c
			}
		}
	}

	var orphans []*zun.Capsule
	for _, c := range capsules {
		if c == keep {
			continue
		}
		if age := time.Since(c.CreatedAt.Time); !c.CreatedAt.IsZero() && age < orphanGrace {
			// Too young to judge: it may belong to a pod this process has not
			// seen yet.
			continue
		}
		orphans = append(orphans, c)
	}
	return orphans
}

func (p *Provider) orphanLoop(ctx context.Context) {
	t := time.NewTicker(orphanSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweepOrphans(ctx)
		}
	}
}
