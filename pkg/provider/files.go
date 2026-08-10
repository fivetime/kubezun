package provider

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// resolveFiles reads the content of a pod's configMap and secret volumes.
//
// A capsule has no kubelet to project these, so their content has to travel
// with it. That also means it is a snapshot: a change to the ConfigMap after
// this point does not reach a running capsule, where a real kubelet would
// eventually refresh the file.
//
// A volume that cannot be read fails the pod rather than being skipped. The
// container would otherwise start and report Ready with the file its
// application reads simply absent — a failure that surfaces as application
// misbehaviour with nothing pointing back here.
func (p *Provider) resolveFiles(ctx context.Context, pod *corev1.Pod) (map[string]map[string][]byte, error) {
	var out map[string]map[string][]byte

	for _, v := range pod.Spec.Volumes {
		var (
			files map[string][]byte
			err   error
		)
		switch {
		case v.ConfigMap != nil:
			files, err = p.configMapFiles(ctx, pod.Namespace, v.ConfigMap)
		case v.Secret != nil:
			files, err = p.secretFiles(ctx, pod.Namespace, v.Secret)
		case serviceAccountVolume(&v):
			if !wantsServiceAccountToken(pod) {
				continue
			}
			var expiry time.Time
			files, expiry, err = p.serviceAccountFiles(ctx, pod)
			if err == nil {
				p.trackTokenExpiry(pod, v.Name, expiry)
			}
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if files == nil {
			// An optional source that does not exist. Kubernetes mounts an
			// empty directory; nothing is written here, and a mount naming this
			// volume simply produces no files.
			continue
		}
		if out == nil {
			out = make(map[string]map[string][]byte)
		}
		out[v.Name] = files
	}
	return out, nil
}

func (p *Provider) configMapFiles(ctx context.Context, namespace string, src *corev1.ConfigMapVolumeSource) (map[string][]byte, error) {
	if p.objects == nil {
		return nil, fmt.Errorf(
			"volume from ConfigMap %s: this node was started without a way to read objects",
			src.Name)
	}
	cm, err := p.objects.ConfigMap(ctx, namespace, src.Name)
	if err != nil {
		if apierrors.IsNotFound(err) && optional(src.Optional) {
			return nil, nil
		}
		return nil, fmt.Errorf("volume from ConfigMap %s/%s: %w", namespace, src.Name, err)
	}

	all := make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
	for k, v := range cm.Data {
		all[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		all[k] = v
	}
	return selectKeys(all, src.Items, fmt.Sprintf("ConfigMap %s/%s", namespace, src.Name))
}

func (p *Provider) secretFiles(ctx context.Context, namespace string, src *corev1.SecretVolumeSource) (map[string][]byte, error) {
	if p.objects == nil {
		return nil, fmt.Errorf(
			"volume from Secret %s: this node was started without a way to read objects",
			src.SecretName)
	}
	sec, err := p.objects.Secret(ctx, namespace, src.SecretName)
	if err != nil {
		if apierrors.IsNotFound(err) && optional(src.Optional) {
			return nil, nil
		}
		return nil, fmt.Errorf("volume from Secret %s/%s: %w", namespace, src.SecretName, err)
	}
	return selectKeys(sec.Data, src.Items, fmt.Sprintf("Secret %s/%s", namespace, src.SecretName))
}

// selectKeys applies a volume's items list, which both chooses which keys are
// mounted and renames them.
func selectKeys(all map[string][]byte, items []corev1.KeyToPath, source string) (map[string][]byte, error) {
	if len(items) == 0 {
		return all, nil
	}
	out := make(map[string][]byte, len(items))
	for _, item := range items {
		data, ok := all[item.Key]
		if !ok {
			// Kubernetes refuses to start the pod in this case, and so does
			// this: the file the mount names would never appear.
			return nil, fmt.Errorf("%s has no key %q", source, item.Key)
		}
		name := item.Path
		if name == "" {
			name = item.Key
		}
		out[name] = data
	}
	return out, nil
}

func optional(v *bool) bool { return v != nil && *v }
