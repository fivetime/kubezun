package zun

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
)

// Label keys written onto every capsule this provider owns. They are the only
// evidence of ownership: capsule names are opaque, and a capsule created by the
// tenant through the Zun API directly must never be mistaken for a pod.
const (
	LabelManagedBy = "knaas.io/managed-by"
	LabelNamespace = "knaas.io/pod-namespace"
	LabelPodName   = "knaas.io/pod-name"
	LabelPodUID    = "knaas.io/pod-uid"

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
		case v.Projected != nil:
			// The service account token volume is injected into every pod that
			// does not opt out, so name the opt-out here: without it the
			// message reads as a defect in the workload rather than a setting
			// the tenant has to make.
			if strings.HasPrefix(v.Name, "kube-api-access-") {
				return unsupported("the service account token volume",
					"a capsule cannot refresh a bound token; set "+
						"automountServiceAccountToken: false on the pod")
			}
			return unsupported("spec.volumes[].projected",
				"projected volumes need a kubelet to refresh their contents")
		}
	}
	for _, cs := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range cs {
			if sc := c.SecurityContext; sc != nil {
				if sc.Privileged != nil && *sc.Privileged {
					return unsupported("securityContext.privileged",
						"the capsule API cannot express privileged containers")
				}
			}
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

	// Probes are passed through as Kubernetes declares them. What a capsule
	// can execute is decided on the Zun side, which is the only place with a
	// path into the container: neither this process nor the compute host can
	// reach a capsule's address, and a kata sandbox's network namespace holds
	// only a tap device, so every probe ultimately runs through ExecSync.
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
	Containers     []container `json:"containers"`
	InitContainers []container `json:"initContainers,omitempty"`
	RestartPolicy  string      `json:"restartPolicy,omitempty"`
}

type template struct {
	Kind             string   `json:"kind"`
	CapsuleVersion   string   `json:"capsuleVersion"`
	Metadata         metadata `json:"metadata"`
	Spec             spec     `json:"spec"`
	Nets             []net    `json:"nets,omitempty"`
	AvailabilityZone string   `json:"availabilityZone,omitempty"`
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
			Name: CapsuleName(pod),
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNamespace: pod.Namespace,
				LabelPodName:   pod.Name,
				LabelPodUID:    string(pod.UID),
			},
		},
		Spec:             spec{RestartPolicy: restartPolicy(pod)},
		AvailabilityZone: opts.AvailabilityZone,
	}

	switch {
	case opts.PortID != "":
		t.Nets = []net{{Port: opts.PortID}}
	case opts.NetworkID != "":
		t.Nets = []net{{Network: opts.NetworkID}}
	}

	for _, c := range pod.Spec.InitContainers {
		built, err := buildContainer(c)
		if err != nil {
			return nil, err
		}
		t.Spec.InitContainers = append(t.Spec.InitContainers, built)
	}
	for _, c := range pod.Spec.Containers {
		built, err := buildContainer(c)
		if err != nil {
			return nil, err
		}
		t.Spec.Containers = append(t.Spec.Containers, built)
	}
	if len(t.Spec.Containers) == 0 {
		return nil, errdefs.InvalidInput("pod has no containers")
	}

	return json.Marshal(t)
}

func buildContainer(c corev1.Container) (container, error) {
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
