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
	// ⚠️ A cache that has been started but has not finished its first list
	// answers NotFound for every pod, which this sweep reads as "the pod is
	// gone". The library registers this provider's callback -- where the sweep
	// is started -- before it waits for that sync, so an unguarded sweep runs
	// against an empty cache. On a restart every capsule is already past the
	// grace period, so the first sweep would delete the entire node's
	// workload; the slow API server that makes a sync take minutes is the same
	// one that makes the restart happen. Waiting costs an unclean capsule a
	// couple more minutes.
	if p.podsSynced != nil && !p.podsSynced() {
		log.G(ctx).Debug("orphan sweep skipped: the pod cache has not synced yet")
		return
	}

	// One walk per tenant: a capsule listing is scoped to the project its
	// credential authenticated as, so "everything this process manages" is the
	// union of one listing per tenant — and each tenant's orphans are deleted
	// with that tenant's own credential, never another's.
	err := p.capsules.Each(ctx, func(_ string, api *zun.CapsuleAPI) error {
		managed, err := api.ListManagedAll(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Warn("orphan sweep skipped one tenant: could not list capsules")
			return nil // this tenant only; the walk continues
		}
		for key, capsules := range managed {
			for _, capsule := range p.orphansAmong(ctx, key, capsules) {
				log.G(ctx).WithField("pod", key).WithField("capsule", capsule.Name()).
					Info("deleting orphaned capsule")
				if err := api.Delete(ctx, capsule.UUID); err != nil {
					log.G(ctx).WithError(err).WithField("capsule", capsule.Name()).
						Warn("could not delete orphaned capsule")
				}
			}
		}
		return nil
	})
	if err != nil {
		log.G(ctx).WithError(err).Warn("orphan sweep did not finish")
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

	// Capsules another virtual node created are not this one's to judge. Its
	// pod informer is filtered to its own node, so it cannot see the pods
	// behind them and every one would read as an orphan (DESIGN §4).
	//
	// A capsule carrying no node name predates the label. Leaving it alone
	// leaks the tenant's quota; deleting it could destroy another node's
	// running workload. Neither is free, so it is left to a human.
	//
	// ⚠️ Reported once per capsule, not once per sweep. Every two minutes
	// forever is 700 identical lines a day for the same two objects, which
	// does not escalate anything -- it teaches whoever reads the log that
	// this line means nothing. Measured at 192 in one day here.
	var mine []*zun.Capsule
	for _, c := range capsules {
		switch c.NodeName() {
		case p.cfg.NodeName:
			mine = append(mine, c)
		case "":
			if p.reportedUnowned(c.UUID) {
				break
			}
			log.G(ctx).WithField("pod", key).WithField("capsule", c.Name()).
				Info("orphan sweep left a capsule alone: it names no node, so " +
					"whether it belongs to this node cannot be established. " +
					"It holds quota until somebody decides; this is said once " +
					"per capsule, not once per sweep")
		}
	}
	capsules = mine
	if len(capsules) == 0 {
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

// reportedUnowned records that a capsule with no owner has been mentioned, and
// says whether it already had been.
func (p *Provider) reportedUnowned(uuid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unowned == nil {
		p.unowned = map[string]struct{}{}
	}
	if _, seen := p.unowned[uuid]; seen {
		return true
	}
	p.unowned[uuid] = struct{}{}
	return false
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
