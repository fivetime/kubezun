package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/fivetime/kubezun/pkg/zun"
)

// CapsuleSubnets resolves a member's subnet from the capsule backing its pod.
//
// A capsule reports the subnet each of its addresses came from, so nothing has
// to be configured and nothing has to be inferred from the address: a tenant
// whose pods sit on more than one subnet still gets each member bound to the
// right one.
type CapsuleSubnets struct {
	capsules *zun.CapsuleAPI

	// Cached because a pool is rebuilt whenever any endpoint changes, which
	// re-resolves every member; a capsule's subnet does not change while it
	// exists. Keyed by namespace/name, so a pod recreated under the same name
	// would read a stale entry — see Forget.
	mu    sync.RWMutex
	cache map[string]string
}

func NewCapsuleSubnets(capsules *zun.CapsuleAPI) *CapsuleSubnets {
	return &CapsuleSubnets{capsules: capsules, cache: map[string]string{}}
}

// SubnetFor returns the subnet the pod's capsule address lives on.
func (c *CapsuleSubnets) SubnetFor(ctx context.Context, namespace, podName string) (string, error) {
	key := zun.PodKey(namespace, podName)

	c.mu.RLock()
	cached, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		return cached, nil
	}

	managed, err := c.capsules.ListManaged(ctx)
	if err != nil {
		return "", fmt.Errorf("listing capsules to resolve the subnet of %s: %w", key, err)
	}
	capsule, ok := managed[key]
	if !ok {
		return "", fmt.Errorf("no capsule for pod %s, so its subnet is unknown", key)
	}
	subnet := zun.PodSubnetID(capsule)
	if subnet == "" {
		return "", fmt.Errorf(
			"capsule for pod %s reports no subnet; a member bound to the wrong "+
				"subnet drops traffic silently, so this is refused rather than guessed", key)
	}

	c.mu.Lock()
	c.cache[key] = subnet
	c.mu.Unlock()
	return subnet, nil
}

// Forget drops a pod's cached subnet. The pod controller calls this when a pod
// goes away, so a pod recreated under the same name — every StatefulSet
// restart — is resolved afresh rather than from the dead pod's capsule.
func (c *CapsuleSubnets) Forget(namespace, podName string) {
	c.mu.Lock()
	delete(c.cache, zun.PodKey(namespace, podName))
	c.mu.Unlock()
}
