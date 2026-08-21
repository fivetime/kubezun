package provider

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fivetime/kubezun/pkg/zun"
)

const sec = int64(time.Second)

// The runtime reports a cumulative counter; a rate needs two readings of it.
func TestCPURateNeedsTwoReadings(t *testing.T) {
	r := newRates()
	key := "ns/pod/c"

	if _, ok := r.observe(key, zun.ContainerStats{
		ContainerID: "abc", Timestamp: 10 * sec, CPUUsageCoreNanoseconds: 1_000_000_000,
	}); ok {
		t.Error("the first reading produced a rate; there was nothing to compare it with")
	}

	// One core busy for one second of wall clock is one core: 1e9 nanocores.
	got, ok := r.observe(key, zun.ContainerStats{
		ContainerID: "abc", Timestamp: 11 * sec, CPUUsageCoreNanoseconds: 2_000_000_000,
	})
	if !ok {
		t.Fatal("the second reading produced no rate")
	}
	if got != 1_000_000_000 {
		t.Errorf("rate = %d nanocores, want 1e9 (one core)", got)
	}

	// Half a core over two seconds.
	got, ok = r.observe(key, zun.ContainerStats{
		ContainerID: "abc", Timestamp: 13 * sec, CPUUsageCoreNanoseconds: 3_000_000_000,
	})
	if !ok || got != 500_000_000 {
		t.Errorf("rate = %d, %v; want 5e8 nanocores", got, ok)
	}
}

// A restart resets the counter. Comparing across one produces either a negative
// rate or, unsigned, an enormous one — and an enormous CPU reading is exactly
// what makes an autoscaler add replicas for a workload that is idle.
func TestARestartDoesNotProduceAPhantomSpike(t *testing.T) {
	r := newRates()
	key := "ns/pod/c"

	r.observe(key, zun.ContainerStats{
		ContainerID: "old", Timestamp: 10 * sec, CPUUsageCoreNanoseconds: 900_000_000_000,
	})
	if got, ok := r.observe(key, zun.ContainerStats{
		ContainerID: "new", Timestamp: 11 * sec, CPUUsageCoreNanoseconds: 5_000_000,
	}); ok {
		t.Errorf("a rate of %d was reported across a restart", got)
	}

	// And the reading after that, both from the new container, is fine.
	if _, ok := r.observe(key, zun.ContainerStats{
		ContainerID: "new", Timestamp: 12 * sec, CPUUsageCoreNanoseconds: 105_000_000,
	}); !ok {
		t.Error("no rate after two readings of the new container")
	}
}

// Same container id, counter still lower than before, or a clock that did not
// move: neither can produce a meaningful rate, and both must be declined
// rather than wrapped around.
func TestNonsenseReadingsAreDeclined(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second zun.ContainerStats
	}{
		{"counter went backwards", zun.ContainerStats{
			ContainerID: "abc", Timestamp: 11 * sec, CPUUsageCoreNanoseconds: 500_000_000}},
		{"clock did not move", zun.ContainerStats{
			ContainerID: "abc", Timestamp: 10 * sec, CPUUsageCoreNanoseconds: 2_000_000_000}},
		{"clock went backwards", zun.ContainerStats{
			ContainerID: "abc", Timestamp: 9 * sec, CPUUsageCoreNanoseconds: 2_000_000_000}},
	} {
		r := newRates()
		r.observe("k", zun.ContainerStats{
			ContainerID: "abc", Timestamp: 10 * sec, CPUUsageCoreNanoseconds: 1_000_000_000})
		if got, ok := r.observe("k", tc.second); ok {
			t.Errorf("%s: reported %d nanocores", tc.name, got)
		}
	}
}

// The map is keyed per container and would otherwise grow for the life of the
// process, one entry per pod ever scheduled here.
func TestReadingsForDepartedPodsAreDropped(t *testing.T) {
	r := newRates()
	r.observe("ns/gone/c", zun.ContainerStats{ContainerID: "a", Timestamp: sec})
	r.observe("ns/here/c", zun.ContainerStats{ContainerID: "b", Timestamp: sec})

	r.forget(map[string]bool{"ns/here/c": true})

	if _, ok := r.last["ns/gone/c"]; ok {
		t.Error("a pod that left the node still has a reading")
	}
	if _, ok := r.last["ns/here/c"]; !ok {
		t.Error("a pod still running lost its reading; its next rate would be missed")
	}
}

// On restart the in-memory map is empty, so the node controller calls
// CreatePod for every pod already running here. Resetting their status then
// publishes "not ready" for the whole node: the EndpointSlice controller
// empties the Services, the reconcilers write empty member sets, and the
// tenant's traffic stops until the sync loop puts it back a few seconds later.
func TestAdoptingAPodKeepsItsPublishedStatus(t *testing.T) {
	p := &Provider{pods: map[string]*corev1.Pod{}, deleted: map[string]types.UID{}}

	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web", UID: "u1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			PodIP: "10.0.0.5",
		},
	}
	p.adoptPod(running)

	got := p.pods["ns/web"]
	if got.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	if got.Status.PodIP != "10.0.0.5" {
		t.Errorf("address lost: %q", got.Status.PodIP)
	}
	var ready corev1.ConditionStatus
	for _, c := range got.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status
		}
	}
	if ready != corev1.ConditionTrue {
		t.Error("readiness was dropped; every Service on this node would lose its endpoints")
	}

	// A pod nothing was ever published for has no readiness to keep.
	fresh := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "new", UID: "u2"}}
	p.adoptPod(fresh)
	if p.pods["ns/new"].Status.Phase != corev1.PodPending {
		t.Errorf("phase = %q, want Pending", p.pods["ns/new"].Status.Phase)
	}
}

// TestMetricsResourceOmitsEmptyFamilies pins the scrape-killing shape: the
// prometheus text encoder rejects a family with zero metrics, and the API
// layer turns that into a 500 — so a tenant whose only pod is restarting
// broke metrics-server's whole scrape of the node.
func TestMetricsResourceOmitsEmptyFamilies(t *testing.T) {
	p := &Provider{
		pods:     map[string]*corev1.Pod{},
		deleted:  map[string]types.UID{},
		capsules: noCapsules{},
		cpuRates: newRates(),
	}
	fams, err := p.GetMetricsResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if len(f.Metric) == 0 {
			t.Fatalf("family %q has no metrics; the text encoder rejects this "+
				"and every scrape of the node returns 500", f.GetName())
		}
	}
	// The node families must survive: a scrape of an idle node is empty of
	// containers, not empty of answers.
	if len(fams) < 2 {
		t.Fatalf("expected at least the two node families, got %d", len(fams))
	}
}

// noCapsules is the idle-node backend: no tenants, no capsules, no calls.
type noCapsules struct{}

func (noCapsules) For(context.Context, string) (*zun.CapsuleAPI, error) {
	return nil, context.Canceled
}
func (noCapsules) Each(context.Context, func(string, *zun.CapsuleAPI) error) error {
	return nil
}
func (noCapsules) TenantOf(string) (string, bool) { return "", false }
