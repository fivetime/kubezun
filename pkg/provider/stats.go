package provider

import (
	"context"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/fivetime/kubezun/pkg/zun"
)

// reading is one container's last CPU counter, kept so the next one can be
// turned into a rate.
type reading struct {
	at         int64 // nanoseconds, the runtime's clock
	coreNanos  uint64
	generation string // container id: a restart makes a new one and resets the counter
}

// rates turns cumulative CPU counters into per-second usage.
//
// The counter resets when a container restarts, so a reading is only comparable
// with an earlier one from the same container id. Without that check the first
// reading after a restart looks like a counter that went backwards, and a naive
// subtraction produces either a negative rate or, unsigned, an enormous one —
// which is exactly the input that makes an autoscaler add replicas.
type rates struct {
	mu   sync.Mutex
	last map[string]reading // key: namespace/pod/container
}

func newRates() *rates { return &rates{last: map[string]reading{}} }

// observe records a reading and returns the rate in nanocores, plus whether
// there was anything to compare against.
func (r *rates) observe(key string, s zun.ContainerStats) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev, had := r.last[key]
	r.last[key] = reading{at: s.Timestamp, coreNanos: s.CPUUsageCoreNanoseconds, generation: s.ContainerID}

	if !had || prev.generation != s.ContainerID {
		return 0, false
	}
	if s.Timestamp <= prev.at || s.CPUUsageCoreNanoseconds < prev.coreNanos {
		// Time did not move, or the counter did go backwards despite the same
		// container id. Neither can produce a meaningful rate.
		return 0, false
	}
	elapsed := uint64(s.Timestamp - prev.at)
	used := s.CPUUsageCoreNanoseconds - prev.coreNanos
	// nanocores = nanoseconds of core time per second of wall clock.
	return used * uint64(time.Second) / elapsed, true
}

// forget drops readings for pods this node no longer runs, so the map does not
// grow for the life of the process.
func (r *rates) forget(live map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.last {
		if !live[k] {
			delete(r.last, k)
		}
	}
}

// podStats collects usage for every pod on this node.
func (p *Provider) podStats(ctx context.Context) []statsv1alpha1.PodStats {
	p.mu.RLock()
	tracked := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		tracked = append(tracked, pod)
	}
	p.mu.RUnlock()

	// One list for the whole node, then one stats call per pod. Zun has no
	// endpoint that answers for a node, but it does not need one list per pod
	// either — the capsules carry the pod they belong to.
	managed, err := p.capsules.ListManaged(ctx)
	if err != nil {
		return nil
	}

	now := metav1.NewTime(time.Now())
	live := map[string]bool{}
	out := make([]statsv1alpha1.PodStats, 0, len(tracked))

	for _, pod := range tracked {
		cap, ok := managed[zun.PodKey(pod.Namespace, pod.Name)]
		if !ok || cap.UUID == "" || cap.PodUID() != string(pod.UID) {
			continue
		}
		stats, err := p.capsules.Stats(ctx, cap.UUID)
		if err != nil || len(stats) == 0 {
			// Between creation and placement there is nothing to report. Skipping
			// one pod beats failing the collection and leaving the node with no
			// metrics at all.
			continue
		}

		ps := statsv1alpha1.PodStats{
			PodRef: statsv1alpha1.PodReference{
				Name: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID),
			},
			StartTime: now,
		}
		if pod.Status.StartTime != nil {
			ps.StartTime = *pod.Status.StartTime
		}

		var podCores, podMemory uint64
		var podCoresKnown bool
		for i := range stats {
			s := &stats[i]
			// Zun renames containers, so position is what ties a capsule
			// container back to the pod's — the same invariant the status path
			// relies on.
			name := s.Name
			if i < len(pod.Spec.Containers) {
				name = pod.Spec.Containers[i].Name
			}
			key := pod.Namespace + "/" + pod.Name + "/" + name
			live[key] = true

			cs := statsv1alpha1.ContainerStats{
				Name:      name,
				StartTime: ps.StartTime,
				Memory: &statsv1alpha1.MemoryStats{
					Time:            metav1.NewTime(time.Unix(0, s.Timestamp)),
					WorkingSetBytes: proto.Uint64(s.MemoryWorkingSetBytes),
					UsageBytes:      proto.Uint64(s.MemoryUsageBytes),
				},
				CPU: &statsv1alpha1.CPUStats{
					Time:                 metav1.NewTime(time.Unix(0, s.Timestamp)),
					UsageCoreNanoSeconds: proto.Uint64(s.CPUUsageCoreNanoseconds),
				},
			}
			if nanocores, ok := p.cpuRates.observe(key, *s); ok {
				cs.CPU.UsageNanoCores = proto.Uint64(nanocores)
				podCores += nanocores
				podCoresKnown = true
			}
			podMemory += s.MemoryWorkingSetBytes
			ps.Containers = append(ps.Containers, cs)
		}

		ps.Memory = &statsv1alpha1.MemoryStats{
			Time: now, WorkingSetBytes: proto.Uint64(podMemory),
		}
		ps.CPU = &statsv1alpha1.CPUStats{Time: now}
		if podCoresKnown {
			ps.CPU.UsageNanoCores = proto.Uint64(podCores)
		}
		out = append(out, ps)
	}

	p.cpuRates.forget(live)
	return out
}

// GetStatsSummary answers the kubelet summary API: what every pod on this node
// is using.
//
// The node's own figures are the sum of its pods. A real kubelet reports the
// machine, including everything outside the pods; there is no machine here,
// and reporting one would mean inventing numbers for hardware this node does
// not have.
func (p *Provider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	pods := p.podStats(ctx)
	now := metav1.NewTime(time.Now())

	var cores, memory uint64
	var coresKnown bool
	for i := range pods {
		if pods[i].CPU != nil && pods[i].CPU.UsageNanoCores != nil {
			cores += *pods[i].CPU.UsageNanoCores
			coresKnown = true
		}
		if pods[i].Memory != nil && pods[i].Memory.WorkingSetBytes != nil {
			memory += *pods[i].Memory.WorkingSetBytes
		}
	}

	node := statsv1alpha1.NodeStats{
		NodeName:  p.cfg.NodeName,
		StartTime: now,
		Memory:    &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: proto.Uint64(memory)},
		CPU:       &statsv1alpha1.CPUStats{Time: now},
	}
	if coresKnown {
		node.CPU.UsageNanoCores = proto.Uint64(cores)
	}

	return &statsv1alpha1.Summary{Node: node, Pods: pods}, nil
}

// GetMetricsResource answers /metrics/resource, which is what metrics-server
// actually scrapes — the summary API above is the older path, and serving only
// it leaves "kubectl top" empty on a current cluster.
//
// Only the cumulative CPU counter is published here, deliberately: the scraper
// computes the rate from two scrapes of its own, and it is better at it than a
// rate derived from whenever this node last happened to be asked.
func (p *Provider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	pods := p.podStats(ctx)

	var containerCPU, containerMem, containerStart []*dto.Metric
	var nodeCPU, nodeMem uint64

	for i := range pods {
		ps := &pods[i]
		for j := range ps.Containers {
			c := &ps.Containers[j]
			labels := []*dto.LabelPair{
				label("container", c.Name),
				label("namespace", ps.PodRef.Namespace),
				label("pod", ps.PodRef.Name),
			}
			if c.CPU != nil && c.CPU.UsageCoreNanoSeconds != nil {
				containerCPU = append(containerCPU, &dto.Metric{
					Label:       labels,
					Counter:     &dto.Counter{Value: proto.Float64(float64(*c.CPU.UsageCoreNanoSeconds) / 1e9)},
					TimestampMs: proto.Int64(c.CPU.Time.UnixMilli()),
				})
				nodeCPU += *c.CPU.UsageCoreNanoSeconds
			}
			if c.Memory != nil && c.Memory.WorkingSetBytes != nil {
				containerMem = append(containerMem, &dto.Metric{
					Label:       labels,
					Gauge:       &dto.Gauge{Value: proto.Float64(float64(*c.Memory.WorkingSetBytes))},
					TimestampMs: proto.Int64(c.Memory.Time.UnixMilli()),
				})
				nodeMem += *c.Memory.WorkingSetBytes
			}
			containerStart = append(containerStart, &dto.Metric{
				Label: labels,
				Gauge: &dto.Gauge{Value: proto.Float64(float64(c.StartTime.Unix()))},
			})
		}
	}

	now := time.Now().UnixMilli()
	return []*dto.MetricFamily{
		family("node_cpu_usage_seconds_total", dto.MetricType_COUNTER,
			"Cumulative cpu time consumed by this node", []*dto.Metric{{
				Counter: &dto.Counter{Value: proto.Float64(float64(nodeCPU) / 1e9)}, TimestampMs: &now,
			}}),
		family("node_memory_working_set_bytes", dto.MetricType_GAUGE,
			"Current working set of this node", []*dto.Metric{{
				Gauge: &dto.Gauge{Value: proto.Float64(float64(nodeMem))}, TimestampMs: &now,
			}}),
		family("container_cpu_usage_seconds_total", dto.MetricType_COUNTER,
			"Cumulative cpu time consumed by a container", containerCPU),
		family("container_memory_working_set_bytes", dto.MetricType_GAUGE,
			"Current working set of a container", containerMem),
		family("container_start_time_seconds", dto.MetricType_GAUGE,
			"Start time of a container since the epoch", containerStart),
	}, nil
}

func family(name string, kind dto.MetricType, help string, metrics []*dto.Metric) *dto.MetricFamily {
	return &dto.MetricFamily{
		Name: proto.String(name), Help: proto.String(help),
		Type: kind.Enum(), Metric: metrics,
	}
}

func label(name, value string) *dto.LabelPair {
	return &dto.LabelPair{Name: proto.String(name), Value: proto.String(value)}
}
