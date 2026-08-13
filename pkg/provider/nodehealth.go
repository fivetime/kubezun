package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	knode "github.com/fivetime/kubezun/pkg/node"
	"github.com/fivetime/kubezun/pkg/zun"
)

const (
	// healthProbeInterval is how often Zun is checked. It is short because
	// what it gates is whether the scheduler keeps sending work here.
	healthProbeInterval = 15 * time.Second

	// unhealthyAfter is how many consecutive failures are needed before the
	// node is taken out of service. A single failed call is usually a blip;
	// flipping the node on one would evict and reschedule every pod on it.
	unhealthyAfter = 3
)

// NodeHealth reports whether this node can still do its job, which for a
// virtual node means one thing: can it reach Zun.
//
// Without this the node stays Ready no matter what, and the scheduler keeps
// placing pods on a node that cannot create a single capsule — they sit in
// ContainerCreating until somebody notices. There is no machine here to fail,
// so the node's health is exactly the health of this process's path to Zun.
type NodeHealth struct {
	capsules *zun.CapsuleAPI

	mu       sync.Mutex
	failures int
	healthy  bool
	lastErr  error
	notify   func(*corev1.Node)
	node     *corev1.Node
}

// NewNodeHealth builds a node provider that tracks Zun reachability.
func NewNodeHealth(capsules *zun.CapsuleAPI, node *corev1.Node) *NodeHealth {
	return &NodeHealth{
		capsules: capsules,
		healthy:  true,
		node:     node,
		notify:   func(*corev1.Node) {},
	}
}

// Ping reports whether the node should stay in service. Returning an error
// makes the node controller stop renewing the lease, which is what tells the
// cluster this node is gone rather than merely idle.
func (h *NodeHealth) Ping(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.healthy {
		return nil
	}
	return fmt.Errorf("zun is unreachable: %w", h.lastErr)
}

// NotifyNodeStatus registers the callback used to push node condition changes.
func (h *NodeHealth) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	h.mu.Lock()
	h.notify = cb
	h.mu.Unlock()
	go h.probeLoop(ctx)
}

func (h *NodeHealth) probeLoop(ctx context.Context) {
	t := time.NewTicker(healthProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.probe(ctx)
		}
	}
}

func (h *NodeHealth) probe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeInterval)
	defer cancel()

	// Listing capsules is the same call the status sync makes, so a node that
	// passes this check can also do the work it is being kept in service for.
	_, err := h.capsules.ListManaged(probeCtx)

	h.mu.Lock()
	was := h.healthy
	if err == nil {
		h.failures = 0
		h.healthy = true
		h.lastErr = nil
	} else {
		h.failures++
		h.lastErr = err
		if h.failures >= unhealthyAfter {
			h.healthy = false
		}
	}
	now, healthy, lastErr, notify := h.healthy, h.healthy, h.lastErr, h.notify
	node := h.node
	h.mu.Unlock()

	if was == now || node == nil {
		return
	}

	updated := node.DeepCopy()
	if healthy {
		log.G(ctx).Info("zun is reachable again; returning the node to service")
		updated.Status.Conditions = []corev1.NodeCondition{knode.ReadyCondition()}
	} else {
		log.G(ctx).WithError(lastErr).
			Error("zun is unreachable; taking the node out of service")
		updated.Status.Conditions = []corev1.NodeCondition{
			knode.NotReadyCondition("zun is unreachable: " + lastErr.Error()),
		}
	}
	for i := range updated.Status.Conditions {
		updated.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(time.Now())
	}

	h.mu.Lock()
	h.node = updated
	h.mu.Unlock()
	notify(updated)
}
