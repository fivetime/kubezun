package provider

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
)

// TokenMinter asks the API server for a ServiceAccount token bound to one pod.
//
// A function rather than a client so the provider keeps its "no caches, one
// object at a time" shape, and so a deployment that does not want to hand out
// tokens can leave it nil.
type TokenMinter func(ctx context.Context, namespace, serviceAccount string,
	req *authv1.TokenRequest) (*authv1.TokenRequest, error)

// serviceAccountVolume reports whether a volume is the one Kubernetes injects
// to give a pod its API credentials.
//
// The name is the contract: kubelet, the admission plugin that creates it and
// every tool that reads it all agree on the "kube-api-access-" prefix.
func serviceAccountVolume(v *corev1.Volume) bool {
	return v.Projected != nil && strings.HasPrefix(v.Name, "kube-api-access-")
}

// tokenLifetime is what is asked for. The API server clamps it to its own
// maximum, and what comes back is what the refresh is scheduled against, so
// asking for more than allowed costs nothing.
//
// It is deliberately not the hour a kubelet asks for. A kubelet rewrites the
// file in place; the refresh here has to reach into a running capsule, so
// every renewal is a chance to fail. Fewer renewals is a smaller target,
// bounded by not wanting a stolen token to be useful for long.
const tokenLifetime = 24 * time.Hour

// refreshAt is when a token should be replaced: well before it expires, so a
// failed attempt has room to be retried before anything breaks.
func refreshAt(expiry time.Time) time.Time {
	life := time.Until(expiry)
	if life <= 0 {
		return time.Now()
	}
	return expiry.Add(-life / 5) // at 80% of its life, as a kubelet does
}

// serviceAccountFiles renders the three files a pod expects to find at
// /var/run/secrets/kubernetes.io/serviceaccount.
//
// Kubernetes projects them from three different places -- a freshly minted
// bound token, the cluster's CA from a ConfigMap, and the namespace as a plain
// string. There is no kubelet here to do the projecting, so they are read and
// rendered once and travel with the capsule.
//
// The token is bound to this pod, not just to the ServiceAccount: it names the
// pod as its object, so it stops working the moment the pod is gone. A
// long-lived ServiceAccount Secret would have been far easier and would have
// handed every capsule a credential that outlives it.
func (p *Provider) serviceAccountFiles(ctx context.Context, pod *corev1.Pod) (map[string][]byte, time.Time, error) {
	if p.tokens == nil {
		return nil, time.Time{}, fmt.Errorf(
			"the pod wants a service account token but this node was started without permission to request one")
	}

	account := pod.Spec.ServiceAccountName
	if account == "" {
		account = "default"
	}
	seconds := int64(tokenLifetime.Seconds())
	req := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			// No audiences: the API server's own is the default, which is what
			// a client built from this token talks to.
			ExpirationSeconds: &seconds,
			BoundObjectRef: &authv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			},
		},
	}
	got, err := p.tokens(ctx, pod.Namespace, account, req)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf(
			"requesting a token for service account %s/%s: %w", pod.Namespace, account, err)
	}

	files := map[string][]byte{
		"token": []byte(got.Status.Token),
		// The namespace as the tenant wrote it, not as this cluster stores it.
		// A client reads this file and puts it straight back into a request;
		// the gateway then prefixes it again, so the stored name asks about
		// "<tenant>-<tenant>-default" and finds nothing. Same mistake, same
		// direction, as the resolver search list.
		"namespace": []byte(p.tenantNamespace(pod.Namespace)),
	}
	if p.objects != nil {
		// The CA the API server presents. Without it a client either refuses to
		// connect or is told to stop checking, and "stop checking" is not a
		// default worth shipping.
		if cm, err := p.objects.ConfigMap(ctx, pod.Namespace, "kube-root-ca.crt"); err == nil {
			if ca, ok := cm.Data["ca.crt"]; ok {
				files["ca.crt"] = []byte(ca)
			}
		}
	}
	return files, got.Status.ExpirationTimestamp.Time, nil
}

// tokenExpiry remembers when each pod's token runs out, so the refresh loop
// knows which one to renew next.
type tokenExpiry struct {
	pod    string // namespace/name
	volume string
	at     time.Time
}

// mountPathOf finds where a volume is mounted in a pod, which is where the
// refreshed token has to be written.
func mountPathOf(pod *corev1.Pod, volume string) string {
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == volume {
				return m.MountPath
			}
		}
	}
	return ""
}

// wantsServiceAccountToken reports whether the pod actually asked for API
// credentials.
//
// Kubernetes' own ServiceAccount admission injects the volume before any
// webhook runs, so a policy that later sets automountServiceAccountToken
// cannot remove what is already there. The field is the pod's stated intent
// and wins over the volume's presence: a pod that opted out gets no token,
// rather than being refused for carrying a volume it did not ask for.
func wantsServiceAccountToken(pod *corev1.Pod) bool {
	return pod.Spec.AutomountServiceAccountToken == nil ||
		*pod.Spec.AutomountServiceAccountToken
}

// trackTokenExpiry records when a pod's token has to be replaced.
func (p *Provider) trackTokenExpiry(pod *corev1.Pod, volume string, at time.Time) {
	if at.IsZero() {
		return
	}
	key := pod.Namespace + "/" + pod.Name
	p.mu.Lock()
	p.tokenExpiries[key] = tokenExpiry{pod: key, volume: volume, at: at}
	p.mu.Unlock()
}

// tokenLoop replaces tokens before they expire.
//
// A kubelet rewrites the file on disk and the container follows it. There is
// no kubelet here and no way in from the container's side either: exec needs a
// shell, and the images most worth running do not have one -- a distroless
// image has no /bin/sh, so the pods that would suffer most are exactly the
// ones it cannot reach. So the file is rewritten from the node holding the
// capsule, through the Zun endpoint that writes a file volume in place.
//
// A failure is retried on the next tick rather than escalated. The token is
// replaced at 80% of its life, which leaves a fifth of it -- hours, at the
// lifetime asked for -- to keep trying before anything stops working.
func (p *Provider) tokenLoop(ctx context.Context) {
	t := time.NewTicker(tokenRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshDueTokens(ctx)
		}
	}
}

const tokenRefreshInterval = 5 * time.Minute

func (p *Provider) refreshDueTokens(ctx context.Context) {
	now := time.Now()

	p.mu.RLock()
	due := make([]tokenExpiry, 0, len(p.tokenExpiries))
	for _, e := range p.tokenExpiries {
		if now.After(refreshAt(e.at)) {
			due = append(due, e)
		}
	}
	p.mu.RUnlock()

	for _, e := range due {
		if err := p.refreshToken(ctx, e); err != nil {
			log.G(ctx).WithError(err).WithField("pod", e.pod).
				Warn("could not renew the service account token; will retry")
		}
	}
}

func (p *Provider) refreshToken(ctx context.Context, e tokenExpiry) error {
	p.mu.RLock()
	pod := p.pods[e.pod]
	p.mu.RUnlock()
	if pod == nil {
		// Gone from this node; nothing to renew and nothing to remember.
		p.mu.Lock()
		delete(p.tokenExpiries, e.pod)
		p.mu.Unlock()
		return nil
	}

	mount := mountPathOf(pod, e.volume)
	if mount == "" {
		return fmt.Errorf("volume %s is mounted nowhere in this pod", e.volume)
	}

	files, expiry, err := p.serviceAccountFiles(ctx, pod)
	if err != nil {
		return err
	}
	token, ok := files["token"]
	if !ok {
		return fmt.Errorf("the new token was empty")
	}

	api, err := p.capsules.For(ctx, pod.Namespace)
	if err != nil {
		return err
	}
	capsules, err := api.ListManaged(ctx)
	if err != nil {
		return err
	}
	cap, ok := capsules[e.pod]
	if !ok || cap.UUID == "" {
		return fmt.Errorf("no capsule is running for this pod")
	}

	// The mount path is the directory; the token is one file inside it, laid
	// out the way buildFileVolumes names them.
	if err := api.UpdateFile(ctx, cap.UUID, path.Join(mount, "token"), token); err != nil {
		return err
	}

	p.trackTokenExpiry(pod, e.volume, expiry)
	log.G(ctx).WithField("pod", e.pod).WithField("expires", expiry).
		Info("renewed the service account token")
	return nil
}
