package zun

import (
	"context"

	"github.com/gophercloud/gophercloud/v2"
)

// ContainerStats is one container's resource usage as the runtime accounts it.
//
// CPU is cumulative, not a rate: nanoseconds of core time since the container
// started, read at Timestamp. A rate needs two readings, and which earlier
// reading belongs to this container is something only the caller knows —
// across a restart the counter goes back to zero, and a rate computed from
// readings that straddle one is nonsense.
type ContainerStats struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	// Timestamp is nanoseconds since the epoch, the runtime's own clock.
	Timestamp int64 `json:"timestamp"`
	// CPUUsageCoreNanoseconds is cumulative core time.
	CPUUsageCoreNanoseconds uint64 `json:"cpu_usage_core_nanoseconds"`
	// MemoryWorkingSetBytes is what a kubelet reports and what an eviction
	// decision is made on: resident memory minus what is reclaimable.
	MemoryWorkingSetBytes uint64 `json:"memory_working_set_bytes"`
	// MemoryUsageBytes is the larger figure, including reclaimable page cache.
	MemoryUsageBytes uint64 `json:"memory_usage_bytes"`
}

// Stats reads the resource usage of every container in a capsule.
//
// Zun answers per container rather than per capsule, which is the shape the
// runtime accounts in and the shape "which container is using the memory"
// needs. A capsule that has not been placed yet has no usage to report and
// answers with an error rather than zeroes, which would read as running and
// using nothing.
func (a *CapsuleAPI) Stats(ctx context.Context, id string) ([]ContainerStats, error) {
	var body struct {
		Stats []ContainerStats `json:"stats"`
	}
	if _, err := a.client.ServiceClient().Get(ctx, a.url(id, "stats"), &body,
		&gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return nil, translate(err)
	}
	return body.Stats, nil
}
