package zun

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label keys written onto every capsule this provider owns. They are the only
// evidence of ownership: capsule names are opaque, and a capsule created by the
// tenant through the Zun API directly must never be mistaken for a pod.
const (
	LabelManagedBy = "knaas.io/managed-by"
	LabelNamespace = "knaas.io/pod-namespace"
	LabelPodName   = "knaas.io/pod-name"
	LabelPodUID    = "knaas.io/pod-uid"

	// LabelOwnerUID is the UID of the workload the pod belongs to -- its
	// controller ownerReference: the StatefulSet, the ReplicaSet. The Zun
	// scheduler's anti-affinity weigher groups by it (DESIGN §4.5).
	//
	// ⚠️ Not the pod name and not the pod UID: keeper-0 and keeper-1 differ in
	// both, and the point is that they are the same thing three times. A pod
	// with no controller gets no label, which the weigher reads as "no
	// opinion" -- a bare pod has no replicas to spread.
	LabelOwnerUID = "knaas.io/owner-uid"
	// LabelNodeName records which virtual node created the capsule. A tenant
	// runs one node per architecture, so several providers share a namespace,
	// and each one's pod informer is filtered to its own node — meaning a
	// provider cannot see the pods behind another node's capsules. Without
	// this label those capsules look like orphans and get deleted out from
	// under a running workload.
	LabelNodeName = "knaas.io/node-name"

	ManagedByValue = "kubezun"
)

// CapsuleName derives the Zun capsule name from a pod. The pod UID is used
// rather than "<namespace>-<name>": names collide across namespaces, and a
// collision would let one tenant's pod address another's capsule.
func CapsuleName(pod *corev1.Pod) string {
	return "kubezun-" + string(pod.UID)
}

// unsupported reports a pod field this provider cannot honour. Silently
// dropping such a field is worse than failing: the pod would run with weaker
// isolation or without storage the workload assumes it has.
func unsupported(field, why string) error {
	return errdefs.InvalidInputf(
		"pod field %s is not supported on a KNaaS virtual node: %s", field, why)
}

// Validate rejects pods whose spec cannot be represented as a capsule.
func Validate(pod *corev1.Pod) error {
	if err := validateSecurity(pod); err != nil {
		return err
	}

	if pod.Spec.HostNetwork {
		return unsupported("spec.hostNetwork",
			"each capsule owns a Neutron port and has no host network to share")
	}
	if pod.Spec.HostPID || pod.Spec.HostIPC {
		return unsupported("spec.hostPID/spec.hostIPC",
			"a capsule runs in its own microVM with no host namespaces to join")
	}
	if pod.Spec.NodeName != "" && pod.Spec.SchedulerName == "" {
		// A pod that named the node itself never went through the scheduler.
		return unsupported("spec.nodeName",
			"pods must be scheduled onto this node, not bound to it directly")
	}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.HostPath != nil:
			return unsupported("spec.volumes[].hostPath",
				"there is no shared host filesystem behind a virtual node")
		case v.ConfigMap != nil, v.Secret != nil:
			// Rendered into the capsule as files; see TemplateOptions.Files.
		case v.EmptyDir != nil:
			// Scratch space shared by the capsule's containers. Carried as its
			// own kind of volume rather than as files: there is nothing to
			// carry, and what a container writes there has to survive being
			// written.
		case v.PersistentVolumeClaim != nil:
			// Resolved by the provider to the storage behind the claim; the
			// capsule is told the Cinder volume or the NFS export, never the
			// claim's name, which means nothing on that side.
		case v.Projected != nil:
			if strings.HasPrefix(v.Name, "kube-api-access-") {
				// Kubernetes' own ServiceAccount admission injects this volume
				// before any webhook runs, so a policy that sets
				// automountServiceAccountToken later cannot remove what is
				// already there. The field is the pod's stated intent: honour
				// it and drop the volume rather than refusing a pod that did
				// opt out.
				if pod.Spec.AutomountServiceAccountToken != nil &&
					!*pod.Spec.AutomountServiceAccountToken {
					continue
				}
				// Carried in as files, like a configMap or secret, and
				// renewed in place before it expires. See
				// pkg/provider/satoken.go.
				continue
			}
			return unsupported("spec.volumes[].projected",
				"projected volumes need a kubelet to refresh their contents")
		default:
			// Refused rather than ignored. A dropped volume leaves the pod
			// Running and Ready while the file its application reads is simply
			// absent, which is the hardest kind of failure to trace back here.
			return unsupported("volume "+v.Name,
				"only configMap and secret volumes can be carried into a capsule")
		}
	}
	for _, cs := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range cs {
			for _, p := range []*corev1.Probe{
				c.LivenessProbe, c.ReadinessProbe, c.StartupProbe,
			} {
				if err := validateProbe(p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateProbe rejects probe forms a capsule cannot execute. Everything runs
// inside the container through ExecSync, so a probe aimed at another address
// than the container's own would silently test the wrong thing.
func validateProbe(p *corev1.Probe) error {
	if p == nil {
		return nil
	}
	switch {
	case p.Exec != nil:
		return nil
	case p.HTTPGet != nil:
		if h := p.HTTPGet.Host; h != "" && h != "localhost" && h != "127.0.0.1" {
			return unsupported("probe httpGet.host",
				"a probe runs inside the container and can only reach the "+
					"container itself; leave host empty")
		}
		return nil
	case p.TCPSocket != nil:
		if h := p.TCPSocket.Host; h != "" && h != "localhost" && h != "127.0.0.1" {
			return unsupported("probe tcpSocket.host",
				"a probe runs inside the container and can only reach the "+
					"container itself; leave host empty")
		}
		return nil
	case p.GRPC != nil:
		return nil
	}
	return unsupported("probe", "no handler is set on the probe")
}

// container is one entry of the capsule template's spec.containers.
type container struct {
	// Named after the pod's container so status can be matched back to it;
	// without a name Zun invents one and the pod's container statuses stay
	// stuck at ContainerCreating however healthy the capsule is.
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Command   []string          `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	WorkDir   string            `json:"workDir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Resources *resources        `json:"resources,omitempty"`
	Ports     []port            `json:"ports,omitempty"`
	Mounts    []volumeMount     `json:"volumeMounts,omitempty"`

	// Probes are passed through as Kubernetes declares them. What a capsule
	// can execute is decided on the Zun side, which is the only place with a
	// path into the container: neither this process nor the compute host can
	// reach a capsule's address, and a kata sandbox's network namespace holds
	// only a tap device, so every probe ultimately runs through ExecSync.
	// Security is what of the pod's securityContext the runtime can be told.
	// On the container rather than in the capsule's annotations because the API
	// overwrites a capsule container's name with a generated one, so an
	// annotation keyed by container name never matches — measured: the
	// container ran as root with a writable root filesystem and nothing said
	// so, which is exactly what these fields exist to prevent.
	Security *containerSecurity `json:"securityContext,omitempty"`

	LivenessProbe  *corev1.Probe `json:"livenessProbe,omitempty"`
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
	StartupProbe   *corev1.Probe `json:"startupProbe,omitempty"`
}

type resources struct {
	// Zun reads this key, but the values are Kubernetes *limits*: the CRI
	// driver turns memory into a cgroup hard limit, so a request would let a
	// container be killed below the ceiling its spec promises.
	Requests resourceValues `json:"requests"`
}

type resourceValues struct {
	CPU    float64 `json:"cpu,omitempty"`
	Memory int64   `json:"memory,omitempty"`
}

type port struct {
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type net struct {
	Network string `json:"network,omitempty"`
	Port    string `json:"port,omitempty"`
}

type metadata struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type spec struct {
	Containers     []container     `json:"containers"`
	InitContainers []container     `json:"initContainers,omitempty"`
	RestartPolicy  string          `json:"restartPolicy,omitempty"`
	Volumes        []capsuleVolume `json:"volumes,omitempty"`
}

// capsuleVolume is one volume of a capsule: a file whose content travels with
// it, or a directory that lives and dies with it.
//
// Zun's local volume driver writes a single file per volume, so a configMap
// with three keys becomes three of these.
type capsuleVolume struct {
	Name     string        `json:"name"`
	File     *fileData     `json:"file,omitempty"`
	EmptyDir *emptyDirData `json:"emptyDir,omitempty"`
	Cinder   *cinderData   `json:"cinder,omitempty"`
	NFS      *nfsData      `json:"nfs,omitempty"`
}

// cinderData names an existing Cinder volume. The capsule never creates one:
// provisioning belongs to the claim's reconciler, so a pod retried five times
// attaches the same volume five times rather than creating five.
type cinderData struct {
	VolumeID string `json:"volumeID"`
	// FSGroup is the pod's fsGroup; the volume is chowned to it after
	// mounting, the way a kubelet does. Without it a fresh filesystem is
	// root's and a non-root workload cannot write to its own volume.
	FSGroup int64 `json:"fsGroup,omitempty"`
}

// nfsData is where a shared filesystem is mounted from.
type nfsData struct {
	Export string `json:"export"`
	// ShareID lets the node mounting this grant itself access with the
	// request's own token -- the tenant's, so a node can only manage access
	// to shares of the tenant whose capsule it is placing.
	ShareID string `json:"shareID,omitempty"`
	FSGroup int64  `json:"fsGroup,omitempty"`
}

// emptyDirData is the pod's emptyDir, passed through as it was written.
type emptyDirData struct {
	// Medium is "Memory" for a tmpfs, empty for a directory on the node.
	Medium string `json:"medium,omitempty"`
	// SizeLimit in bytes, enforced only for a tmpfs -- the kernel does it
	// there. On a node directory nothing enforces it, and the capsule API says
	// so rather than accepting a limit that does not hold.
	SizeLimit int64 `json:"sizeLimit,omitempty"`
}

type fileData struct {
	// Base64 so a value with newlines, or one that is not text at all, arrives
	// as it was stored.
	Contents string `json:"contents"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type template struct {
	Kind             string   `json:"kind"`
	CapsuleVersion   string   `json:"capsuleVersion"`
	Metadata         metadata `json:"metadata"`
	Spec             spec     `json:"spec"`
	Nets             []net    `json:"nets,omitempty"`
	SecurityGroups   []string `json:"securityGroups,omitempty"`
	AvailabilityZone string   `json:"availabilityZone,omitempty"`
	Architecture     string   `json:"architecture,omitempty"`
	Runtime          string   `json:"runtime,omitempty"`
}

// annotationsFor carries settings the capsule needs but has no field for.
func annotationsFor(opts TemplateOptions) map[string]string {
	out := map[string]string{}
	if len(opts.DNSSearches) > 0 {
		out["knaas.io/dns-searches"] = strings.Join(opts.DNSSearches, ",")
	}
	if len(opts.DNSNameservers) > 0 {
		out["knaas.io/dns-nameservers"] = strings.Join(opts.DNSNameservers, ",")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// knownArchitectures are the machines Zun can place a capsule on. The
// Kubernetes spelling is what appears here and in the node's arch label; Zun
// normalises it to the Linux one (arm64 -> aarch64) on the way to Placement.
var knownArchitectures = map[string]struct{}{
	"amd64": {}, "arm64": {}, "ppc64le": {}, "s390x": {}, "riscv64": {},
}

// ValidateArchitecture rejects an architecture Zun cannot place. Catching this
// at startup matters: a node labelled with an architecture no host reports
// still registers and still accepts pods, which then sit Pending with nothing
// in their events pointing at the typo.
func ValidateArchitecture(arch string) error {
	if arch == "" {
		return nil
	}
	if _, ok := knownArchitectures[arch]; !ok {
		return fmt.Errorf("unknown architecture %q: want one of amd64, arm64, ppc64le, s390x, riscv64", arch)
	}
	return nil
}

// TemplateOptions carries the placement decisions the provider makes for a pod.
type TemplateOptions struct {
	// NetworkID is the tenant Neutron network capsules attach to. Leaving it
	// empty makes Zun pick the first network it finds, which on a tenant with
	// more than one network is a coin flip.
	NetworkID string
	// PortID pins the capsule to a pre-created Neutron port so its IP survives
	// a recreate (DESIGN §14.2). Takes precedence over NetworkID.
	PortID string
	// AvailabilityZone maps the virtual node's topology zone onto Zun.
	AvailabilityZone string
	// NodeName is the virtual node creating the capsule; it is stamped on the
	// capsule so another node serving the same namespace can tell whose it is.
	NodeName string
	// Files is the resolved content of the pod's configMap and secret volumes,
	// keyed by volume name and then by file name within the volume. A capsule
	// has no kubelet to project them, so their content travels with it.
	Files map[string]map[string][]byte
	// DNSSearches is the resolver search list the pod's containers get, which
	// is what lets an application use a Service's short name. Only this side
	// knows it: it is derived from the pod's namespace.
	DNSSearches []string
	// DNSNameservers is which resolver the pod's containers ask. For a tenant
	// behind the gateway this is their own resolver, which is the only one that
	// answers with the names their manifests use.
	DNSNameservers []string

	// SecurityGroups is what the capsule's port should carry, decided from the
	// NetworkPolicies that select this pod.
	//
	// ⚠️ Named at creation, not attached afterwards. The groups are what make a
	// pod reachable at all -- a tenant's pods reach each other because they
	// share a group, not because they share a network -- so a capsule that
	// comes up with the wrong ones is a capsule that is briefly reachable by
	// the wrong people, or briefly reachable by nobody.
	//
	// ⚠️ An empty list is not the same as none. None lets Zun fall through to
	// the project's default group; empty is the deny-all a pod isolated in both
	// directions is supposed to have. The distinction is carried by omitempty
	// only working on nil, so callers must not "helpfully" replace an empty
	// slice with nil.
	SecurityGroups []string

	// Claims is the storage behind the pod's persistentVolumeClaim volumes,
	// keyed by volume name, resolved by the provider before the template is
	// built. A claim that cannot be resolved fails the pod there; by the time
	// this map is read every claim volume the pod names has an entry.
	Claims map[string]ClaimMount

	// Architecture is the machine this node's capsules must run on. A node
	// serves one architecture: its kubernetes.io/arch label is what got the
	// pod scheduled here, and the capsule has to land on a host that matches
	// or the image will not execute.
	Architecture string
}

// BuildTemplate converts a pod into a Zun capsule template.
func BuildTemplate(pod *corev1.Pod, opts TemplateOptions) ([]byte, error) {
	if err := Validate(pod); err != nil {
		return nil, err
	}

	t := template{
		Kind:           "capsule",
		CapsuleVersion: "beta",
		Metadata: metadata{
			Name:        CapsuleName(pod),
			Annotations: annotationsFor(opts),
			Labels:      ownedLabels(pod),
		},
		Spec:             spec{RestartPolicy: restartPolicy(pod)},
		AvailabilityZone: opts.AvailabilityZone,
		Architecture:     opts.Architecture,
		// The pod's runtimeClassName, verbatim. Before this it was silently
		// dropped -- a tenant asking for the Firecracker tier ran under the
		// node default and nothing anywhere said so. Verbatim on purpose:
		// which handlers exist is the compute node's truth, and a whitelist
		// here would go stale the day the platform adds a tier. A name no
		// node offers fails the capsule with the runtime's own error, which
		// the pod reports -- loud, attributable, and not this layer's guess.
		Runtime: runtimeClass(pod),
	}
	if opts.NodeName != "" {
		// Written only when known: an empty value would claim ownership by a
		// node with no name, and the orphan sweep reads an absent label as
		// "ownership cannot be established" and leaves the capsule alone.
		t.Metadata.Labels[LabelNodeName] = opts.NodeName
	}

	switch {
	case opts.PortID != "":
		t.Nets = []net{{Port: opts.PortID}}
	case opts.NetworkID != "":
		t.Nets = []net{{Network: opts.NetworkID}}
	}
	t.SecurityGroups = opts.SecurityGroups

	for _, c := range pod.Spec.InitContainers {
		built, err := buildContainer(pod, c)
		if err != nil {
			return nil, err
		}
		t.Spec.InitContainers = append(t.Spec.InitContainers, built)
	}
	for _, c := range pod.Spec.Containers {
		built, err := buildContainer(pod, c)
		if err != nil {
			return nil, err
		}
		t.Spec.Containers = append(t.Spec.Containers, built)
	}
	if len(t.Spec.Containers) == 0 {
		return nil, errdefs.InvalidInput("pod has no containers")
	}

	volumes, mounts, err := buildFileVolumes(pod, opts.Files, opts.Claims)
	if err != nil {
		return nil, err
	}
	t.Spec.Volumes = volumes
	for i := range t.Spec.InitContainers {
		t.Spec.InitContainers[i].Mounts = mounts[t.Spec.InitContainers[i].Name]
	}
	for i := range t.Spec.Containers {
		t.Spec.Containers[i].Mounts = mounts[t.Spec.Containers[i].Name]
	}

	return json.Marshal(t)
}

func buildContainer(pod *corev1.Pod, c corev1.Container) (container, error) {
	out := container{
		Name:    c.Name,
		Image:   c.Image,
		Command: c.Command,
		Args:    c.Args,
		WorkDir: c.WorkingDir,
	}
	if len(c.Env) > 0 {
		out.Env = make(map[string]string, len(c.Env))
		for _, e := range c.Env {
			// Values sourced from a ConfigMap, Secret or the downward API are
			// resolved by the caller before this point; anything still unset
			// here has no value to send.
			if e.ValueFrom == nil {
				out.Env[e.Name] = e.Value
			}
		}
	}
	for _, p := range c.Ports {
		out.Ports = append(out.Ports, port{
			ContainerPort: p.ContainerPort,
			Protocol:      string(p.Protocol),
		})
	}
	if r := buildResources(c.Resources); r != nil {
		out.Resources = r
	}
	// Network probes are turned into exec probes against the container itself
	// here rather than in the pod: what Kubernetes stores stays what its author
	// wrote, and only the capsule sees the rewritten form.
	var err error
	if sec := effectiveSecurity(pod, &c); !sec.empty() {
		out.Security = &sec
	}

	if out.LivenessProbe, err = RewriteProbe(c.LivenessProbe, &c); err != nil {
		return out, err
	}
	if out.ReadinessProbe, err = RewriteProbe(c.ReadinessProbe, &c); err != nil {
		return out, err
	}
	if out.StartupProbe, err = RewriteProbe(c.StartupProbe, &c); err != nil {
		return out, err
	}
	return out, nil
}

func buildResources(r corev1.ResourceRequirements) *resources {
	// Limits, not requests: see the comment on the resources type.
	cpu := r.Limits.Cpu()
	mem := r.Limits.Memory()
	if cpu.IsZero() && mem.IsZero() {
		return nil
	}
	out := &resources{}
	if !cpu.IsZero() {
		out.Requests.CPU = float64(cpu.MilliValue()) / 1000.0
	}
	if !mem.IsZero() {
		// Zun expects MiB.
		out.Requests.Memory = mem.Value() / (1024 * 1024)
	}
	return out
}

// runtimeClass is the pod's requested runtime tier, empty when unset.
func runtimeClass(pod *corev1.Pod) string {
	if pod.Spec.RuntimeClassName == nil {
		return ""
	}
	return *pod.Spec.RuntimeClassName
}

// restartPolicy returns the value the capsule template schema accepts. The
// capsule spec uses the Kubernetes spelling ("Always"), unlike the container
// API's own lowercase form, and the schema rejects anything else.
func restartPolicy(pod *corev1.Pod) string {
	switch pod.Spec.RestartPolicy {
	case corev1.RestartPolicyNever:
		return "Never"
	case corev1.RestartPolicyOnFailure:
		return "OnFailure"
	default:
		return "Always"
	}
}

// PodKeyFromLabels recovers the namespace/name a capsule was created for.
func PodKeyFromLabels(labels map[string]string) (namespace, name string, ok bool) {
	if labels[LabelManagedBy] != ManagedByValue {
		return "", "", false
	}
	namespace, name = labels[LabelNamespace], labels[LabelPodName]
	return namespace, name, namespace != "" && name != ""
}

// PodKey formats the key used to index pods in provider state.
func PodKey(namespace, name string) string { return namespace + "/" + name }

// SplitPodKey is the inverse of PodKey.
func SplitPodKey(key string) (namespace, name string, err error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed pod key %q", key)
	}
	return parts[0], parts[1], nil
}

// ClaimMount is the storage behind one persistentVolumeClaim volume, in the
// terms the capsule needs: which service backs it and how to reach it.
type ClaimMount struct {
	// Kind is "cinder" for a block device, "nfs" for a shared filesystem.
	Kind string
	// VolumeID is the Cinder volume, when Kind is "cinder".
	VolumeID string
	// Export is "host:/path", when Kind is "nfs".
	Export string
}

// ownedLabels stamps the capsule with what it is and whose it is.
func ownedLabels(pod *corev1.Pod) map[string]string {
	labels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNamespace: pod.Namespace,
		LabelPodName:   pod.Name,
		LabelPodUID:    string(pod.UID),
	}
	if owner := metav1.GetControllerOf(pod); owner != nil {
		labels[LabelOwnerUID] = string(owner.UID)
	}
	return labels
}
