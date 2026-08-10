package provider

import (
	"testing"
	"time"

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
