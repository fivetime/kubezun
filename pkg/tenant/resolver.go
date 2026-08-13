// Package tenant resolves which OpenStack session a namespace's work belongs
// in.
//
// One process serves several tenants (DESIGN §1.2), so a credential can no
// longer be a value read once at startup. Every call that reaches OpenStack has
// to arrive with the credential of the tenant that owns the object it is
// about — and picking the wrong one does not fail, it succeeds against the
// wrong project. That is why the resolution is here, in one place, rather than
// threaded through each caller.
//
// The chain is namespace → tenant → (project, region) → session.
package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	// ProjectAnnotation records which OpenStack project a tenant is bound to.
	// It is the declared binding; the token says what the credential actually
	// is, and the two are compared on every connect (see checkBinding).
	ProjectAnnotation = "knaas.io/project-id"
	// RegionAnnotation records the region half of the same binding. A
	// namespace's volumes and networks do not cross regions, so a credential
	// that resolves another region's endpoints is as wrong as one for another
	// project — and looks just as correct field by field.
	RegionAnnotation = "knaas.io/region"

	// The transitional per-tenant configuration, carried on the same Secret
	// until the Tenant CRD controller owns it (DESIGN §4.6). These are not
	// part of the binding check: changing a network is reconfiguration, not
	// a rebind, and refusing on it would block legitimate changes.
	NetworkIDAnnotation    = "knaas.io/network-id"
	VIPSubnetIDAnnotation  = "knaas.io/vip-subnet-id"
	VIPNetworkIDAnnotation = "knaas.io/vip-network-id"
)

// Binding is one tenant's resolved identity and configuration: the session
// its calls go out on, and the per-tenant values that used to be process
// flags. One process serving several tenants cannot take a tenant's network
// as a flag — every tenant has their own.
type Binding struct {
	Session *Session
	// NetworkID is the tenant Neutron network capsules attach to.
	NetworkID string
	// VIPSubnetID and VIPNetworkID are where the tenant's Service load
	// balancer addresses come from.
	VIPSubnetID  string
	VIPNetworkID string
}

// Credentials names the Secret keys a tenant's credential is read from. They
// are the standard OS_* names so the same content works in a file, an
// environment, and here.
const (
	keyAuthURL    = "OS_AUTH_URL"
	keyAppCredID  = "OS_APPLICATION_CREDENTIAL_ID"
	keyAppCredNam = "OS_APPLICATION_CREDENTIAL_NAME"
	keyAppCredSec = "OS_APPLICATION_CREDENTIAL_SECRET"
	keyUsername   = "OS_USERNAME"
	keyUserDomain = "OS_USER_DOMAIN_NAME"
	keyRegion     = "OS_REGION_NAME"
)

// Resolver hands out one session per tenant, built on first use and kept.
//
// ⚠️ The Secret lives in a namespace of the platform's, never in the tenant's.
// A Secret does not have to be visible to be used: a pod spec naming it in a
// volume gets its contents, and that path never passes through the gateway that
// would filter it. A pod can only mount a Secret from its own namespace, which
// is a rule of Kubernetes rather than a filter of ours (DESIGN §4.6.2).
type Resolver struct {
	// Secrets reads credentials, scoped to the platform namespace that holds
	// them. Also written to, once, when a tenant is first bound.
	Secrets corev1client.SecretInterface

	// TenantOf says which tenant a namespace belongs to, and whether the
	// namespace is served at all. Fails closed: an unserved namespace must
	// answer false rather than fall back to a default.
	TenantOf func(namespace string) (string, bool)

	// SecretName is what a tenant's credential Secret is called. Nil names it
	// after the tenant.
	SecretName func(tenant string) string

	// Connect builds a session. Nil uses NewSession; tests replace it.
	Connect func(ctx context.Context, creds Credentials) (*Session, error)

	mu    sync.Mutex
	built map[string]*Binding
	// refused remembers tenants whose binding did not check out, so the
	// refusal is reported once rather than on every pod.
	refused map[string]struct{}
}

// For returns the session a namespace's OpenStack work belongs in.
func (r *Resolver) For(ctx context.Context, namespace string) (*Session, error) {
	b, err := r.BindingFor(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return b.Session, nil
}

// BindingFor returns the session and the per-tenant configuration together.
func (r *Resolver) BindingFor(ctx context.Context, namespace string) (*Binding, error) {
	tenant, served := r.TenantOf(namespace)
	if !served {
		// Not this process's namespace. Deliberately not an error about
		// credentials: the caller's authorization check should have caught it,
		// and saying "no credential" here would read as a configuration
		// problem rather than a boundary being enforced.
		return nil, fmt.Errorf("namespace %q is not served by this process", namespace)
	}
	if tenant == "" {
		// Served, but carrying no tenant label. Falling back to any configured
		// default would create this namespace's capsules in somebody else's
		// project, so there is no fallback.
		return nil, fmt.Errorf("namespace %q names no tenant, so no credential can be chosen for it", namespace)
	}

	r.mu.Lock()
	if b, ok := r.built[tenant]; ok {
		r.mu.Unlock()
		return b, nil
	}
	r.mu.Unlock()

	binding, err := r.connect(ctx, tenant)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Another goroutine may have won the race; keeping the first keeps one
	// session per tenant rather than one per caller.
	if b, ok := r.built[tenant]; ok {
		return b, nil
	}
	if r.built == nil {
		r.built = map[string]*Binding{}
	}
	r.built[tenant] = binding
	return binding, nil
}

func (r *Resolver) secretName(tenant string) string {
	if r.SecretName != nil {
		return r.SecretName(tenant)
	}
	return tenant
}

func (r *Resolver) connect(ctx context.Context, tenant string) (*Binding, error) {
	name := r.secretName(tenant)
	secret, err := r.Secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("tenant %s has no credential: secret %q does not exist", tenant, name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the credential of tenant %s: %w", tenant, err)
	}

	creds := credentialsFrom(secret)
	connect := r.Connect
	if connect == nil {
		connect = NewSession
	}
	client, err := connect(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("authenticating tenant %s: %w", tenant, err)
	}

	if err := r.checkBinding(ctx, tenant, secret, client); err != nil {
		return nil, err
	}
	return &Binding{
		Session:      client,
		NetworkID:    secret.Annotations[NetworkIDAnnotation],
		VIPSubnetID:  secret.Annotations[VIPSubnetIDAnnotation],
		VIPNetworkID: secret.Annotations[VIPNetworkIDAnnotation],
	}, nil
}

func credentialsFrom(s *corev1.Secret) Credentials {
	get := func(k string) string { return string(s.Data[k]) }
	return Credentials{
		AuthURL:               get(keyAuthURL),
		ApplicationCredID:     get(keyAppCredID),
		ApplicationCredName:   get(keyAppCredNam),
		ApplicationCredSecret: get(keyAppCredSec),
		Username:              get(keyUsername),
		UserDomainName:        get(keyUserDomain),
		Region:                get(keyRegion),
	}
}

// checkBinding compares what a credential turned out to be against what this
// tenant is recorded as bound to.
//
// ⚠️ Three states, and the middle one is the trap. Nothing recorded means this
// is the first bind, so the answer is written down. A match is ordinary. A
// mismatch must REFUSE — and the tempting fourth behaviour, overwriting the
// record with what was found, is the bug: it reads like keeping state current
// and is in fact the thing that makes swapping a tenant onto another project
// silent.
//
// ⚠️ What is compared is the project the token authenticated as, not the
// credential material. Rotating an application credential within one project is
// routine and must keep working; only the project changing is a rebind.
//
// Why it matters: with no check, changing a tenant's project takes effect
// immediately and cannot be undone. Capsules in the old project become
// invisible — a listing with the new credential does not return them — so every
// running pod is judged to have lost its capsule and is failed and replaced,
// while the old capsules keep running, keep billing, and can never be reclaimed
// because nothing that can see them is looking (DESIGN §4.6.3).
func (r *Resolver) checkBinding(ctx context.Context, tenant string, secret *corev1.Secret, client *Session) error {
	want := map[string]string{
		ProjectAnnotation: client.Project(),
		RegionAnnotation:  client.Region(),
	}

	var missing []string
	for key, found := range want {
		recorded := secret.Annotations[key]
		switch {
		case recorded == "":
			missing = append(missing, key)
		case found == "":
			// The session could not say. Treated as unknown rather than as a
			// mismatch: refusing on a question we failed to ask would take a
			// working deployment down for a reason it cannot act on.
			log.G(ctx).WithField("tenant", tenant).WithField("binding", key).
				Warn("this credential did not report its binding, so it could not be checked")
		case recorded != found:
			r.reportRefusal(ctx, tenant, key, recorded, found)
			return fmt.Errorf(
				"tenant %s is bound to %s=%s but its credential authenticates as %s; "+
					"refusing to act. Changing the binding is a migration, not an edit: "+
					"drain the tenant under the old credential first, or its existing "+
					"capsules become invisible and unreclaimable",
				tenant, key, recorded, found)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return r.record(ctx, tenant, secret, want, missing)
}

// record writes the binding down the first time a tenant is connected.
//
// A failure here is not fatal: the credential is good and the work can proceed.
// It only means the next start will treat it as a first bind again, so the
// check stays inert until a write succeeds — which is worth a warning and not
// worth refusing to run a tenant over.
func (r *Resolver) record(ctx context.Context, tenant string, secret *corev1.Secret, want map[string]string, missing []string) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{}}}
	annotations := patch["metadata"].(map[string]any)["annotations"].(map[string]string)
	for _, key := range missing {
		if want[key] == "" {
			continue
		}
		annotations[key] = want[key]
	}
	if len(annotations) == 0 {
		return nil
	}
	body, err := marshal(patch)
	if err != nil {
		return nil
	}
	if _, err := r.Secrets.Patch(ctx, secret.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		log.G(ctx).WithError(err).WithField("tenant", tenant).
			Warn("could not record this tenant's binding; the check stays inert until it can be written")
		return nil
	}
	log.G(ctx).WithField("tenant", tenant).WithField("binding", annotations).
		Info("recorded this tenant's OpenStack binding")
	return nil
}

// reportRefusal says a binding did not check out, once per tenant rather than
// once per pod: the sync loop asks for a credential on every pass, and a line
// per pass buries the one line that mattered.
func (r *Resolver) reportRefusal(ctx context.Context, tenant, key, recorded, found string) {
	r.mu.Lock()
	if r.refused == nil {
		r.refused = map[string]struct{}{}
	}
	_, seen := r.refused[tenant]
	r.refused[tenant] = struct{}{}
	r.mu.Unlock()
	if seen {
		return
	}
	log.G(ctx).WithField("tenant", tenant).WithField("binding", key).
		WithField("recorded", recorded).WithField("credential", found).
		Error("refusing to serve this tenant: its credential does not match the " +
			"binding it was onboarded with. This is said once per tenant, not once " +
			"per pod")
}

// marshal is json.Marshal, named so the import stays out of the top of the
// file's story.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
