package volume

import (
	"context"
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

const (
	// KindAnnotation and IDAnnotation record on the PersistentVolume what was
	// created for it, so teardown and the mount path do not have to infer it
	// from the driver name.
	KindAnnotation   = "knaas.io/storage-kind"
	IDAnnotation     = "knaas.io/storage-id"
	ExportAnnotation = "knaas.io/storage-export"

	// provisionerAnnotation is what Kubernetes' own external-provisioner
	// contract uses to mark which provisioner owns a claim. Kept verbatim so a
	// claim of ours reads the way an operator expects.
	provisionerAnnotation = "volume.kubernetes.io/storage-provisioner"
)

// Reconciler turns a tenant's claims into OpenStack storage and back.
type Reconciler struct {
	Backend *Backend

	Claims  corev1listers.PersistentVolumeClaimLister
	Volumes corev1listers.PersistentVolumeLister
	Client  corev1client.CoreV1Interface

	// StorageClass is the class this process answers for. A claim naming
	// another class belongs to someone else and is left alone -- the same
	// boundary the ingress class draws.
	StorageClass string
	// Tenant prefixes names on the OpenStack side, so one project holding
	// several tenants' storage can still be read by a human.
	Tenant string
	// ServesNamespace is the authorization boundary, as everywhere else here.
	ServesNamespace func(namespace string) bool
}

// storageName is what the volume or share is called in OpenStack. It carries
// the claim it belongs to because that is the only question anyone looking at
// a list of volumes actually has.
func (r *Reconciler) storageName(namespace, name string) string {
	if r.Tenant != "" {
		return fmt.Sprintf("kubezun_%s_pvc_%s_%s", r.Tenant, namespace, name)
	}
	return fmt.Sprintf("kubezun_pvc_%s_%s", namespace, name)
}

// pvName is the PersistentVolume's name. Derived from the claim's UID so a
// claim deleted and recreated under the same name never adopts the old volume.
func pvName(claim *corev1.PersistentVolumeClaim) string {
	return "pvc-" + string(claim.UID)
}

// Ours reports whether a claim is this process's to serve.
func (r *Reconciler) Ours(claim *corev1.PersistentVolumeClaim) bool {
	if claim.Spec.StorageClassName == nil {
		return false
	}
	name := *claim.Spec.StorageClassName
	// The gateway prefixes cluster-scoped names for a tenant, StorageClass
	// included, so what the tenant wrote as "knaas" arrives as
	// "<tenant>-knaas". Matching only the bare name would leave every tenant
	// claim unserved -- the same trap the ingress class fell into.
	return name == r.StorageClass ||
		(r.Tenant != "" && name == r.Tenant+"-"+r.StorageClass)
}

// Reconcile provisions storage for one claim and binds a volume to it.
func (r *Reconciler) Reconcile(ctx context.Context, namespace, name string) error {
	if !r.ServesNamespace(namespace) {
		return nil
	}
	claim, err := r.Claims.PersistentVolumeClaims(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil // gone; the sweep deals with what it left behind
	}
	if err != nil {
		return err
	}
	if !r.Ours(claim) || claim.DeletionTimestamp != nil {
		return nil
	}
	if claim.Spec.VolumeName != "" {
		// Already bound. Whether the storage behind it still exists is the
		// sweep's question, not this path's.
		return nil
	}

	kind, err := KindFor(claim.Spec.AccessModes)
	if err != nil {
		return fmt.Errorf("claim %s/%s: %w", namespace, name, err)
	}

	// An existing volume for this claim means a previous attempt got as far as
	// creating one. Binding that rather than making another is what keeps a
	// retry from leaving storage nobody will ever mount and everybody pays for.
	pv, err := r.Volumes.Get(pvName(claim))
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		pv, err = r.provision(ctx, claim, kind)
		if err != nil {
			return err
		}
	}

	if pv.Status.Phase == corev1.VolumeAvailable || pv.Status.Phase == "" {
		log.G(ctx).WithField("claim", namespace+"/"+name).
			WithField("volume", pv.Name).Info("storage is ready for this claim")
	}
	return nil
}

func (r *Reconciler) provision(ctx context.Context, claim *corev1.PersistentVolumeClaim, kind Kind) (*corev1.PersistentVolume, error) {
	request := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	gib := int((request.Value() + (1 << 30) - 1) / (1 << 30))

	made, err := r.Backend.Create(ctx, r.storageName(claim.Namespace, claim.Name), kind, gib,
		fmt.Sprintf("kubezun claim %s/%s", claim.Namespace, claim.Name))
	if err != nil {
		return nil, err
	}

	// A share is only reachable once Manila has published where it lives. The
	// export is recorded when it appears; until then the volume is still
	// created, so a second attempt does not make a second share.
	export := ""
	if made.Kind == Shared {
		export, _ = r.Backend.ExportOf(ctx, made.ID)
	}

	size := resource.NewQuantity(int64(made.GiB)<<30, resource.BinarySI)
	driver := BlockDriver
	if made.Kind == Shared {
		driver = SharedDriver
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName(claim),
			Annotations: map[string]string{
				KindAnnotation:        string(made.Kind),
				IDAnnotation:          made.ID,
				ExportAnnotation:      export,
				provisionerAnnotation: driver,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: claim.Spec.AccessModes,
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: *size},
			// Deleted with the claim unless the tenant says otherwise. Storage
			// that outlives what asked for it is storage nobody is watching
			// and everybody is paying for.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              *claim.Spec.StorageClassName,
			// Bound to this claim by name, so nothing else can take it in the
			// window before the claim's binding is written.
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", APIVersion: "v1",
				Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       driver,
					VolumeHandle: made.ID,
					VolumeAttributes: map[string]string{
						"export": export,
					},
				},
			},
		},
	}
	if policy := claim.Annotations["knaas.io/reclaim-policy"]; policy == "Retain" {
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	}

	created, err := r.Client.PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return r.Client.PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
	}
	if err != nil {
		// The storage exists and the volume does not, which is the one
		// direction that leaks. Give it back rather than leave it for a sweep
		// that has nothing to recognise it by.
		if rmErr := r.Backend.Delete(ctx, made.Kind, made.ID); rmErr != nil {
			log.G(ctx).WithError(rmErr).WithField("id", made.ID).
				Warn("could not remove the storage of a volume that failed to be created")
		}
		return nil, fmt.Errorf("creating the volume for claim %s/%s: %w",
			claim.Namespace, claim.Name, err)
	}
	return created, nil
}

// SweepReleased removes the storage behind volumes whose claim is gone.
//
// The API server marks such a volume Released and, for a Delete policy, waits
// for whoever provisioned it to act. Nothing else here will: there is no CSI
// controller in this cluster, which is the whole reason this package exists.
func (r *Reconciler) SweepReleased(ctx context.Context) {
	all, err := r.Volumes.List(labels.Everything())
	if err != nil {
		return
	}
	for _, pv := range all {
		if !r.provisionedByUs(pv) {
			continue
		}
		if pv.Status.Phase != corev1.VolumeReleased && pv.Status.Phase != corev1.VolumeFailed {
			continue
		}
		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			continue
		}
		kind := Kind(pv.Annotations[KindAnnotation])
		id := pv.Annotations[IDAnnotation]
		if id == "" {
			continue
		}
		log.G(ctx).WithField("volume", pv.Name).WithField("id", id).
			Info("removing the storage of a released volume")
		if err := r.Backend.Delete(ctx, kind, id); err != nil {
			log.G(ctx).WithError(err).WithField("id", id).
				Warn("could not remove it; will retry")
			continue
		}
		if err := r.Client.PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{}); err != nil &&
			!apierrors.IsNotFound(err) {
			log.G(ctx).WithError(err).WithField("volume", pv.Name).
				Warn("the storage is gone but the volume object is not")
		}
	}
}

func (r *Reconciler) provisionedByUs(pv *corev1.PersistentVolume) bool {
	if pv.Spec.CSI == nil {
		return false
	}
	return pv.Spec.CSI.Driver == BlockDriver || pv.Spec.CSI.Driver == SharedDriver
}

// Mount describes how a capsule reaches the storage behind a claim.
type Mount struct {
	Kind Kind
	// ID is the Cinder volume id, for a block device.
	ID string
	// Export is "host:/path", for a shared filesystem.
	Export string
}

// MountFor resolves a pod's claim to what the capsule needs to be told.
func (r *Reconciler) MountFor(namespace, claimName string) (*Mount, error) {
	claim, err := r.Claims.PersistentVolumeClaims(namespace).Get(claimName)
	if err != nil {
		return nil, fmt.Errorf("claim %s/%s: %w", namespace, claimName, err)
	}
	if claim.Spec.VolumeName == "" {
		// Not bound yet. Refusing the pod is right: starting it without the
		// volume leaves an application writing into its own image, which looks
		// like it worked until the capsule is replaced.
		return nil, fmt.Errorf("claim %s/%s has no volume yet", namespace, claimName)
	}
	pv, err := r.Volumes.Get(claim.Spec.VolumeName)
	if err != nil {
		return nil, err
	}
	m := &Mount{
		Kind:   Kind(pv.Annotations[KindAnnotation]),
		ID:     pv.Annotations[IDAnnotation],
		Export: pv.Annotations[ExportAnnotation],
	}
	if m.ID == "" && pv.Spec.CSI != nil {
		m.ID = pv.Spec.CSI.VolumeHandle
	}
	if m.Kind == Shared && m.Export == "" {
		// The share exists but Manila had not published where it lives when
		// the volume was written. Look again rather than mount nothing.
		export, err := r.Backend.ExportOf(context.Background(), m.ID)
		if err != nil || export == "" {
			return nil, fmt.Errorf("share %s has no export location yet", m.ID)
		}
		m.Export = export
	}
	if m.Kind == "" || m.ID == "" {
		return nil, fmt.Errorf("volume %s does not say what backs it", pv.Name)
	}
	return m, nil
}

// ExportHost is the address part of an export location, which is what a Manila
// access rule has to be granted for.
func ExportHost(export string) string {
	if i := strings.Index(export, ":"); i > 0 {
		return export[:i]
	}
	return ""
}
