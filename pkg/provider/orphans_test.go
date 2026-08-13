package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/fivetime/kubezun/pkg/zun"
)

// listerWith builds a pod lister backed by the given pods.
func listerWith(t *testing.T, pods ...*corev1.Pod) corev1listers.PodLister {
	t.Helper()
	objs := make([]any, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	informer := factory.Core().V1().Pods()
	for _, o := range objs {
		if err := informer.Informer().GetIndexer().Add(o); err != nil {
			t.Fatalf("seed lister: %v", err)
		}
	}
	return informer.Lister()
}

func livePod(namespace, name, uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: types.UID(uid),
	}}
}

func capsuleFor(namespace, name, uid string, age time.Duration) *zun.Capsule {
	return &zun.Capsule{
		UUID:      "capsule-" + uid,
		NameField: "kubezun-" + uid,
		CreatedAt: zun.Time{Time: time.Now().Add(-age)},
		LabelsField: map[string]string{
			zun.LabelManagedBy: zun.ManagedByValue,
			zun.LabelNamespace: namespace,
			zun.LabelPodName:   name,
			zun.LabelPodUID:    uid,
			zun.LabelNodeName:  "111111-node-az1",
		},
	}
}

// names returns the capsule names orphansAmong selected for deletion.
func names(capsules []*zun.Capsule) []string {
	out := make([]string, 0, len(capsules))
	for _, c := range capsules {
		out = append(out, c.Name())
	}
	return out
}

func TestOrphanSweepSparesCapsulesWithLivePods(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t, livePod("111111-default", "web", "uid-1"))

	capsule := capsuleFor("111111-default", "web", "uid-1", time.Hour)
	got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{capsule})
	if len(got) != 0 {
		t.Fatalf("a capsule whose pod is alive was selected for deletion: %v", names(got))
	}
}

func TestOrphanSweepDeletesDuplicatesOfALivePod(t *testing.T) {
	// Zun accepts a second capsule under an existing name, so a retried create
	// leaves duplicates that run and bill alongside the real one. Only the
	// newest is kept, since that is the one the pod's status reflects.
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t, livePod("111111-default", "web", "uid-1"))

	older := capsuleFor("111111-default", "web", "uid-1", 2*time.Hour)
	older.NameField = "kubezun-uid-1-older"
	newer := capsuleFor("111111-default", "web", "uid-1", time.Hour)

	got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{older, newer})
	if len(got) != 1 || got[0] != older {
		t.Fatalf("duplicate cleanup kept the wrong capsule: %v", names(got))
	}
}

func TestOrphanSweepDeletesCapsulesWhosePodIsGone(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t)

	capsule := capsuleFor("111111-default", "web", "uid-1", time.Hour)
	if got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{capsule}); len(got) != 1 {
		t.Fatal("a capsule with no pod was kept")
	}
}

func TestOrphanSweepDeletesCapsuleOfRecreatedPod(t *testing.T) {
	// Same pod name, different UID: the capsule belongs to the pod that was
	// replaced, and nothing else will ever delete it.
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t, livePod("111111-default", "web", "uid-new"))

	capsule := capsuleFor("111111-default", "web", "uid-old", time.Hour)
	if got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{capsule}); len(got) != 1 {
		t.Fatal("a capsule left behind by a recreated pod was kept")
	}
}

func TestOrphanSweepSparesYoungCapsules(t *testing.T) {
	// A capsule created moments ago may belong to a pod this process has not
	// seen yet; deleting it would kill a pod that is starting normally.
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t)

	capsule := capsuleFor("111111-default", "web", "uid-1", 5*time.Second)
	if got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{capsule}); len(got) != 0 {
		t.Fatal("a capsule younger than the grace period was selected for deletion")
	}
}

func TestOrphanSweepIgnoresOtherNamespaces(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t)

	capsule := capsuleFor("222222-default", "web", "uid-1", time.Hour)
	if got := p.orphansAmong(t.Context(), "222222-default/web", []*zun.Capsule{capsule}); len(got) != 0 {
		t.Fatal("a capsule from a namespace this node does not serve was selected for deletion")
	}
}

func TestOrphanSweepDoesNothingWithoutAPodCache(t *testing.T) {
	// Without a cache every capsule looks orphaned, so the sweep must not run
	// at all rather than delete the tenant's entire workload.
	p := newTestProvider(t, "111111-default")
	p.podLister = nil
	p.sweepOrphans(t.Context())
}

// A tenant needing more than one architecture runs more than one virtual node
// in the same namespace. Each node's pod informer is filtered to its own node,
// so the pods behind a sibling's capsules are invisible here — and without the
// node label every one of them would be deleted while running.
func TestOrphanSweepSparesAnotherNodesCapsules(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t) // this node sees none of the sibling's pods

	sibling := capsuleFor("111111-default", "web", "uid-1", time.Hour)
	sibling.LabelsField[zun.LabelNodeName] = "111111-node-arm64"

	if got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{sibling}); len(got) != 0 {
		t.Fatalf("deleted another node's running capsule: %v", names(got))
	}
}

// A capsule from before the label cannot be attributed to any node. Deleting it
// risks another node's workload, so it is kept and reported instead.
func TestOrphanSweepSparesUnlabelledCapsules(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.podLister = listerWith(t)

	old := capsuleFor("111111-default", "web", "uid-1", time.Hour)
	delete(old.LabelsField, zun.LabelNodeName)

	if got := p.orphansAmong(t.Context(), "111111-default/web", []*zun.Capsule{old}); len(got) != 0 {
		t.Fatalf("deleted a capsule of unknown ownership: %v", names(got))
	}
}

// countingZun is a stand-in for the capsule API that records whether it was
// reached at all. The sweep's guard is invisible from the outside otherwise:
// "skipped because the cache had not synced" and "ran and found no orphans"
// both end with nothing deleted.
func countingZun(t *testing.T, calls *int) *zun.CapsuleAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"capsules":[]}`)
	}))
	t.Cleanup(srv.Close)
	return zun.NewCapsuleAPIAt(&gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       srv.URL + "/v1/",
	})
}

// TestOrphanSweepWaitsForThePodCache is the restart safety net.
//
// ⚠️ The library registers this provider's pod callback -- where the sweep is
// started -- before it waits for the pod informer to sync
// (podcontroller.go:306 registers, :312 waits). A started-but-unsynced lister
// answers NotFound for every pod, and this sweep reads NotFound as "the pod is
// gone". On a restart every capsule is already past the grace period, so an
// unguarded first sweep deletes the whole node's running workload.
func TestOrphanSweepWaitsForThePodCache(t *testing.T) {
	calls := 0
	p := newTestProvider(t, "111111-default")
	p.capsules = StaticCapsules{API: countingZun(t, &calls)}
	// An empty cache, exactly as an unsynced informer presents itself.
	p.podLister = listerWith(t)
	p.podsSynced = func() bool { return false }

	p.sweepOrphans(t.Context())
	if calls != 0 {
		t.Fatalf("the sweep judged capsules against an unsynced cache: %d calls to Zun", calls)
	}

	// The other half of the criterion: with the cache synced it must actually
	// run, or the test above would pass just as well against a sweep that was
	// broken outright.
	p.podsSynced = func() bool { return true }
	p.sweepOrphans(t.Context())
	if calls == 0 {
		t.Fatal("the sweep never ran even with a synced cache, so the check above proves nothing")
	}
}
