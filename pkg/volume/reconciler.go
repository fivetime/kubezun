package volume

import (
	"context"
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/fivetime/kubezun/pkg/zun"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	storagev1listers "k8s.io/client-go/listers/storage/v1"
	record "k8s.io/client-go/tools/record"
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
// selectedNodeAnnotation is what the scheduler writes on a claim once it has
// placed the first pod that uses it. Not a CSI mechanism: the scheduler's
// volume binding plugin sets it for whatever provisioner owns the class, and
// only consults CSI to ask about published capacity.
const selectedNodeAnnotation = "volume.kubernetes.io/selected-node"

type Reconciler struct {
	Backend *Backend

	Claims  corev1listers.PersistentVolumeClaimLister
	Volumes corev1listers.PersistentVolumeLister
	Classes storagev1listers.StorageClassLister
	Client  corev1client.CoreV1Interface

	// Events reports expansion progress where a tenant looks for it. Nil
	// falls back to the log, which is where nobody looks.
	Events record.EventRecorder

	// Capsules grows the filesystem on a block volume after the volume itself
	// has grown. Nil leaves that half undone, and says so on the claim.
	Capsules *zun.CapsuleAPI

	// Pods finds which capsules are using a claim, since the filesystem has to
	// be grown wherever the volume is attached.
	Pods corev1listers.PodLister

	// PlacementOf says where a virtual node puts what it runs. Nil, or a node
	// it does not recognise, leaves the deployment default in force.
	PlacementOf func(nodeName string) (Placement, bool)

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

// classOf resolves the StorageClass a claim names.
//
// The gateway passes storageClassName through verbatim -- measured by reading
// its code, after the ingress class's prefixing was wrongly assumed to apply
// here too -- so the exact name is the normal case. The prefixed spelling is
// still accepted as tolerance for claims written against the old per-tenant
// naming, not because anything produces it.
func (r *Reconciler) classOf(claim *corev1.PersistentVolumeClaim) *storagev1.StorageClass {
	if claim.Spec.StorageClassName == nil || r.Classes == nil {
		return nil
	}
	name := *claim.Spec.StorageClassName
	if sc, err := r.Classes.Get(name); err == nil {
		return sc
	}
	if r.Tenant != "" && strings.HasPrefix(name, r.Tenant+"-") {
		if sc, err := r.Classes.Get(strings.TrimPrefix(name, r.Tenant+"-")); err == nil {
			return sc
		}
	}
	return nil
}

// Ours reports whether a claim is this process's to serve: one naming a
// StorageClass whose provisioner is one of this package's drivers. The class
// is the catalog entry, and its parameters say which volume or share type to
// buy.
//
// There is deliberately no other way in. An earlier per-tenant magic name
// ("<tenant>-knaas") needed no StorageClass object at all, which also meant
// the tenant could see their own tenant id inside their claim and could learn
// nothing about what the class bought them. Claims bound under it keep
// working -- binding, mounting and reclaim never consult the class again --
// but new claims name a catalog entry or stay Pending.
func (r *Reconciler) Ours(claim *corev1.PersistentVolumeClaim) bool {
	if claim.Spec.StorageClassName == nil {
		return false
	}
	sc := r.classOf(claim)
	return sc != nil &&
		(sc.Provisioner == BlockDriver || sc.Provisioner == SharedDriver)
}

// planFor decides what a claim gets: which service, and which type within it.
//
// A typed class pins the service: ReadWriteMany on a block-device class is
// refused here, loudly, rather than provisioned as something the class did
// not promise.
func (r *Reconciler) planFor(claim *corev1.PersistentVolumeClaim) (Kind, string, error) {
	sc := r.classOf(claim)
	if sc == nil {
		return "", "", fmt.Errorf("claim %s/%s names no catalog class", claim.Namespace, claim.Name)
	}
	switch sc.Provisioner {
	case BlockDriver:
		for _, m := range claim.Spec.AccessModes {
			if m == corev1.ReadWriteMany || m == corev1.ReadOnlyMany {
				return "", "", fmt.Errorf(
					"class %s is a block device and cannot serve %s; use a shared filesystem class",
					sc.Name, m)
			}
		}
		return Block, sc.Parameters["type"], nil
	case SharedDriver:
		return Shared, sc.Parameters["share_type"], nil
	}
	return "", "", fmt.Errorf("class %s is not served here", sc.Name)
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
		// Bound. The only thing left that can change is its size.
		return r.expandIfAsked(ctx, claim)
	}

	kind, storageType, err := r.planFor(claim)
	if err != nil {
		return fmt.Errorf("claim %s/%s: %w", namespace, name, err)
	}

	// A class that waits for its first consumer means exactly this: do not
	// decide yet. The scheduler places the pod, records which node it chose on
	// the claim, and only then is the zone the storage must live in known.
	//
	// ⚠️ What the scheduler chooses is a virtual node, and Zun chooses which
	// machine inside that node's availability zone actually runs the capsule.
	// That is the right granularity anyway: a volume belongs to a zone, not to
	// a machine, so the half Kubernetes cannot see is the half that does not
	// matter here.
	if r.waitingForConsumer(claim) {
		log.G(ctx).WithField("claim", namespace+"/"+name).
			Debug("waiting for a pod to be scheduled before placing this storage")
		return nil
	}

	// An existing volume for this claim means a previous attempt got as far as
	// creating one. Binding that rather than making another is what keeps a
	// retry from leaving storage nobody will ever mount and everybody pays for.
	pv, err := r.Volumes.Get(pvName(claim))
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		pv, err = r.provision(ctx, claim, kind, storageType)
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

// Placement is where a virtual node puts what it runs: the zone it advertises
// to Kubernetes and the availability zone its capsules are created in.
//
// ⚠️ Two names for one place, and they are not the same name. A node can
// advertise topology.kubernetes.io/zone=az1 while its capsules go to the
// OpenStack zone "nova"; handing the Kubernetes name to Cinder asks for a zone
// that does not exist.
type Placement struct {
	Zone string
	AZ   string
}

// waitingForConsumer reports whether this claim's class defers placement and
// the scheduler has not placed a pod yet.
func (r *Reconciler) waitingForConsumer(claim *corev1.PersistentVolumeClaim) bool {
	class := r.classOf(claim)
	if class == nil || class.VolumeBindingMode == nil ||
		*class.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return false
	}
	return claim.Annotations[selectedNodeAnnotation] == ""
}

// placementFor is where this claim's storage should be created.
//
// ⚠️ The availability zone comes from configuration and never from the node's
// compute zone. There is no one OpenStack zone namespace: measured on one
// deployment, Nova and Cinder both call it "nova" while Manila's share service
// lives in "manila-zone-0". Passing the compute zone to a storage service
// therefore works by coincidence where the names happen to agree, and where
// they do not the scheduler answers "no storage could be allocated" -- which
// reads as a full backend rather than as a name from the wrong namespace.
//
// The node still decides the zone written onto the volume, which is what keeps
// a pod from being rescheduled away from storage it cannot reach. Only the
// OpenStack-side name is left to whoever knows it.
func (r *Reconciler) placementFor(claim *corev1.PersistentVolumeClaim) Placement {
	where := Placement{}
	if r.Backend != nil {
		where.AZ = r.Backend.AvailabilityZone
	}
	node := claim.Annotations[selectedNodeAnnotation]
	if node == "" || r.PlacementOf == nil {
		return where
	}
	if p, ok := r.PlacementOf(node); ok {
		where.Zone = p.Zone
	}
	return where
}

// expandIfAsked grows a bound claim whose request has outgrown what it has.
//
// ⚠️ There is no resizer in this deployment, so nothing else will do it and
// nothing else will report it either: with no resizer, Kubernetes does not even
// mark the claim as resizing. Before this existed the edit was accepted and
// then landed nowhere -- a spec asking for more, a status showing the old size,
// and no condition or event to connect the two.
func (r *Reconciler) expandIfAsked(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	want, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return nil
	}
	have := claim.Status.Capacity[corev1.ResourceStorage]
	if want.Cmp(have) <= 0 {
		return nil
	}

	pv, err := r.Volumes.Get(claim.Spec.VolumeName)
	if err != nil {
		return nil // not ours to grow, or not there yet
	}
	if !r.provisionedByUs(pv) {
		return nil
	}
	kind := Kind(pv.Annotations[KindAnnotation])
	id := pv.Annotations[IDAnnotation]
	if id == "" {
		return fmt.Errorf("volume %s records no storage id", pv.Name)
	}

	gib := int((want.Value() + (1 << 30) - 1) / (1 << 30))
	log.G(ctx).WithField("claim", claim.Namespace+"/"+claim.Name).
		WithField("from", have.String()).WithField("to", want.String()).
		Info("growing the storage behind this claim")

	got, err := r.Backend.Expand(ctx, kind, id, gib)
	if err != nil {
		r.report(ctx, claim, "ExpansionFailed", err.Error())
		return err
	}

	// ⚠️ The size it actually has, not the size that was asked for. Recording
	// the request would make a partial expansion look complete, and the
	// difference is only discovered when the filesystem fills.
	actual := resource.NewQuantity(int64(got)<<30, resource.BinarySI)

	// The volume is bigger, so the volume object says so. ⚠️ The claim does
	// not, yet: for a block volume there is a second step, and writing the new
	// size on the claim before it happens makes the request look satisfied --
	// after which nothing comes back, and the filesystem stays small for ever.
	// Leaving the claim at the old size is what makes the next pass retry.
	if err := r.recordVolumeSize(ctx, pv, *actual); err != nil {
		return err
	}
	if kind == Block {
		// ⚠️ The device is bigger and the filesystem inside it is not. The
		// filesystem is mounted on the compute node, so growing it can only
		// happen there -- and until it does, the pod sees the old size however
		// large the volume is, which looks exactly like an expansion that
		// failed.
		if err := r.growFilesystems(ctx, claim, id, got); err != nil {
			r.report(ctx, claim, "FileSystemResizePending",
				"the volume is now "+actual.String()+" but the filesystem has "+
					"not been grown yet: "+err.Error())
			return err
		}
	}
	return r.recordClaimSize(ctx, claim, *actual)
}

// growFilesystems tells every capsule using this volume to grow the filesystem
// on it.
//
// Every one, because ReadWriteOnce is a Kubernetes rule and not a Cinder one:
// a volume can be attached to more than one capsule, and one left with the old
// filesystem is one whose pod cannot use the space it was given.
func (r *Reconciler) growFilesystems(ctx context.Context, claim *corev1.PersistentVolumeClaim, volumeID string, gib int) error {
	if r.Capsules == nil || r.Pods == nil {
		return nil
	}
	pods, err := r.Pods.Pods(claim.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	var failed error
	for _, pod := range pods {
		if !usesClaim(pod, claim.Name) {
			continue
		}
		if err := r.Capsules.ExtendVolume(ctx, string(pod.UID), volumeID, gib); err != nil {
			// Reported, not swallowed: the pod is running on a volume whose
			// filesystem is the old size, and nothing else will notice.
			log.G(ctx).WithError(err).WithField("pod", pod.Namespace+"/"+pod.Name).
				Warn("could not grow the filesystem on this pod's volume")
			failed = err
		}
	}
	return failed
}

func usesClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// recordVolumeSize says how big the storage is. Written as soon as it is true,
// because it is true whether or not the filesystem has caught up.
func (r *Reconciler) recordVolumeSize(ctx context.Context, pv *corev1.PersistentVolume,
	size resource.Quantity) error {
	if pv.Spec.Capacity[corev1.ResourceStorage] == size {
		return nil
	}
	updated := pv.DeepCopy()
	updated.Spec.Capacity[corev1.ResourceStorage] = size
	if _, err := r.Client.PersistentVolumes().Update(ctx, updated,
		metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("recording the new size on %s: %w", pv.Name, err)
	}
	return nil
}

// recordClaimSize says how much space the workload can actually use, which is
// only true once the filesystem has been grown. ⚠️ This is also what stops the
// retry: while it says the old size, the request still looks unsatisfied.
func (r *Reconciler) recordClaimSize(ctx context.Context,
	claim *corev1.PersistentVolumeClaim, size resource.Quantity) error {
	fresh, err := r.Client.PersistentVolumeClaims(claim.Namespace).Get(
		ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if fresh.Status.Capacity == nil {
		fresh.Status.Capacity = corev1.ResourceList{}
	}
	fresh.Status.Capacity[corev1.ResourceStorage] = size
	if _, err := r.Client.PersistentVolumeClaims(claim.Namespace).UpdateStatus(
		ctx, fresh, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("recording the new size on claim %s/%s: %w",
			claim.Namespace, claim.Name, err)
	}
	return nil
}

// report says something about a claim where a tenant will see it.
func (r *Reconciler) report(ctx context.Context, claim *corev1.PersistentVolumeClaim, reason, message string) {
	if r.Events == nil {
		log.G(ctx).WithField("claim", claim.Namespace+"/"+claim.Name).
			WithField("reason", reason).Info(message)
		return
	}
	r.Events.Eventf(claim, corev1.EventTypeNormal, reason, "%s", message)
}

func (r *Reconciler) provision(ctx context.Context, claim *corev1.PersistentVolumeClaim, kind Kind, storageType string) (*corev1.PersistentVolume, error) {
	request := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	gib := int((request.Value() + (1 << 30) - 1) / (1 << 30))

	where := r.placementFor(claim)
	// Where a volume was put, and on whose account. With one zone configured
	// this reads as noise; with two it is the only record of a decision that
	// cannot be revisited, because a volume does not move between zones.
	log.G(ctx).WithField("claim", claim.Namespace+"/"+claim.Name).
		WithField("selected-node", claim.Annotations[selectedNodeAnnotation]).
		WithField("zone", where.Zone).WithField("availability-zone", where.AZ).
		Info("placing storage")
	made, err := r.Backend.Create(ctx, r.storageName(claim.Namespace, claim.Name), kind, gib,
		fmt.Sprintf("kubezun claim %s/%s", claim.Namespace, claim.Name), storageType, where.AZ)
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
	if where.Zone != "" {
		// ⚠️ Without this the volume is placed once and then forgotten: a pod
		// deleted and recreated can be scheduled into another zone, and a
		// volume cannot follow it. The claim stays Bound and the pod stays
		// Pending, with nothing in either object saying why.
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      corev1.LabelTopologyZone,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{where.Zone},
					}},
				}},
			},
		}
	}
	if policy := claim.Annotations["knaas.io/reclaim-policy"]; policy == "Retain" {
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	}

	created, err := r.Client.PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// A concurrent reconcile won: the informer cache had not seen its PV
		// yet, so this one provisioned too. The storage THIS attempt created
		// backs nothing and is recorded nowhere -- the winner's PV names the
		// winner's storage id -- so no sweep will ever recognise it. Losing
		// the race must pay its own bill on the way out.
		if rmErr := r.Backend.Delete(ctx, made.Kind, made.ID); rmErr != nil {
			log.G(ctx).WithError(rmErr).WithField("id", made.ID).
				Warn("could not remove the storage of a lost provisioning race")
		}
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
