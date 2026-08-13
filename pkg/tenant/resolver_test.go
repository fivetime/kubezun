package tenant

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fivetime/kubezun/pkg/zun"
)

// fixture builds a resolver over one platform namespace holding the given
// secrets, with a stubbed connect that reports whatever project and region the
// test wants the credential to turn out to be.
func fixture(t *testing.T, project, region string, secrets ...*corev1.Secret) (*Resolver, *fake.Clientset, *int) {
	t.Helper()
	objs := make([]any, 0, len(secrets))
	for _, s := range secrets {
		objs = append(objs, s)
	}
	client := fake.NewSimpleClientset(toRuntime(objs)...)
	calls := 0
	r := &Resolver{
		Secrets: client.CoreV1().Secrets("knaas-system"),
		TenantOf: func(namespace string) (string, bool) {
			switch namespace {
			case "111111-default", "111111-kube-system":
				return "111111", true
			case "222222-default":
				return "222222", true
			case "no-tenant":
				return "", true
			}
			return "", false
		},
		Connect: func(context.Context, zun.Credentials) (*zun.Client, error) {
			calls++
			return zun.NewClientForTest(project, region), nil
		},
	}
	return r, client, &calls
}

func secretFor(tenant string, annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tenant,
			Namespace:   "knaas-system",
			Annotations: annotations,
		},
		Data: map[string][]byte{
			keyAuthURL:    []byte("http://keystone/identity/v3"),
			keyAppCredID:  []byte("cred-" + tenant),
			keyAppCredSec: []byte("secret"),
			keyRegion:     []byte("RegionOne"),
		},
	}
}

// TestRefusesACredentialThatChangedProject is the one that matters.
//
// ⚠️ Without it, swapping a tenant onto another project is silent and
// irreversible: the old project's capsules stop being listed, so every running
// pod reads as having lost its capsule and is replaced, while the originals keep
// running, keep billing, and can never be reclaimed.
func TestRefusesACredentialThatChangedProject(t *testing.T) {
	r, _, _ := fixture(t, "project-new", "RegionOne",
		secretFor("111111", map[string]string{
			ProjectAnnotation: "project-old",
			RegionAnnotation:  "RegionOne",
		}))

	_, err := r.For(t.Context(), "111111-default")
	if err == nil {
		t.Fatal("a credential for a different project was accepted")
	}
	if !strings.Contains(err.Error(), "project-old") || !strings.Contains(err.Error(), "project-new") {
		t.Errorf("the refusal does not say what disagreed: %v", err)
	}
}

// TestRefusesACredentialThatChangedRegion guards the other half of the binding.
// A region is as load-bearing as the project: volumes and networks do not cross
// one, and every field still looks correct on its own.
func TestRefusesACredentialThatChangedRegion(t *testing.T) {
	r, _, _ := fixture(t, "project-a", "RegionTwo",
		secretFor("111111", map[string]string{
			ProjectAnnotation: "project-a",
			RegionAnnotation:  "RegionOne",
		}))

	if _, err := r.For(t.Context(), "111111-default"); err == nil {
		t.Fatal("a credential resolving another region's endpoints was accepted")
	}
}

// TestFirstBindIsRecorded covers the state the refusal depends on: with nothing
// written down there is nothing to compare against, so the first connect has to
// write it or the check never starts working.
func TestFirstBindIsRecorded(t *testing.T) {
	r, client, _ := fixture(t, "project-a", "RegionOne", secretFor("111111", nil))

	if _, err := r.For(t.Context(), "111111-default"); err != nil {
		t.Fatalf("a first bind was refused: %v", err)
	}
	got, err := client.CoreV1().Secrets("knaas-system").Get(t.Context(), "111111", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[ProjectAnnotation] != "project-a" {
		t.Errorf("the project was not recorded: %+v", got.Annotations)
	}
	if got.Annotations[RegionAnnotation] != "RegionOne" {
		t.Errorf("the region was not recorded: %+v", got.Annotations)
	}
}

// TestRotationWithinAProjectIsAllowed is the case a stricter check would break.
// An application credential expires and is replaced routinely; only the project
// changing is a rebind, so comparing the credential material rather than the
// project would turn every rotation into an outage.
func TestRotationWithinAProjectIsAllowed(t *testing.T) {
	secret := secretFor("111111", map[string]string{
		ProjectAnnotation: "project-a",
		RegionAnnotation:  "RegionOne",
	})
	secret.Data[keyAppCredID] = []byte("a-completely-different-credential")
	secret.Data[keyAppCredSec] = []byte("and-a-new-secret")

	r, _, _ := fixture(t, "project-a", "RegionOne", secret)
	if _, err := r.For(t.Context(), "111111-default"); err != nil {
		t.Fatalf("rotating a credential inside one project was refused: %v", err)
	}
}

// TestOneSessionPerTenantAcrossNamespaces keeps a tenant's namespaces on one
// session. Two sessions would authenticate twice per tenant and, more to the
// point, would let the two halves of one tenant drift apart.
func TestOneSessionPerTenantAcrossNamespaces(t *testing.T) {
	r, _, calls := fixture(t, "project-a", "RegionOne",
		secretFor("111111", map[string]string{
			ProjectAnnotation: "project-a", RegionAnnotation: "RegionOne",
		}))

	first, err := r.For(t.Context(), "111111-default")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.For(t.Context(), "111111-kube-system")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("a tenant's two namespaces got two sessions")
	}
	if *calls != 1 {
		t.Errorf("authenticated %d times for one tenant", *calls)
	}
}

// TestUnservedNamespaceGetsNoCredential is the authorization boundary seen from
// here: the answer for a namespace this process does not serve must be a
// refusal, never a fallback to whatever credential is at hand.
func TestUnservedNamespaceGetsNoCredential(t *testing.T) {
	r, _, _ := fixture(t, "project-a", "RegionOne",
		secretFor("111111", map[string]string{ProjectAnnotation: "project-a"}))

	if _, err := r.For(t.Context(), "someone-elses-namespace"); err == nil {
		t.Fatal("an unserved namespace was given a credential")
	}
	// Served but unlabelled is the subtler one: it is a namespace of ours, so a
	// fallback feels safe, and it would create this namespace's capsules in
	// whichever project happened to be configured.
	if _, err := r.For(t.Context(), "no-tenant"); err == nil {
		t.Fatal("a namespace naming no tenant was given a credential")
	}
}

// TestMissingSecretSaysWhich keeps onboarding legible: a tenant whose Secret
// has not been created yet is the ordinary state during onboarding, and the
// error has to name what is missing rather than fail as an authentication
// problem.
func TestMissingSecretSaysWhich(t *testing.T) {
	r, _, _ := fixture(t, "project-a", "RegionOne")
	_, err := r.For(t.Context(), "222222-default")
	if err == nil {
		t.Fatal("a tenant with no credential secret resolved anyway")
	}
	if !strings.Contains(err.Error(), "222222") {
		t.Errorf("the error does not name the tenant: %v", err)
	}
}

func toRuntime(objs []any) []runtime.Object {
	out := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		if r, ok := o.(runtime.Object); ok {
			out = append(out, r)
		}
	}
	return out
}
