// Package node describes the Kubernetes Node object a tenant's virtual node
// registers as.
package node

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label and taint keys the platform relies on.
const (
	// TenantTaintKey keeps foreign workloads off a tenant's node. The
	// virtual-kubelet default taint is deliberately not used: charts that
	// tolerate everything with operator=Exists would land on this node.
	TenantTaintKey = "knaas.io/tenant"

	// PoolLabelKey is the placement anchor the gateway injects onto tenant
	// pods, so it must match what that transformer writes.
	PoolLabelKey = "kubezoo.io/pool"

	// TypeLabelKey carries the virtual-kubelet marker used by cluster-wide
	// DaemonSets to exclude virtual nodes.
	TypeLabelKey   = "type"
	TypeLabelValue = "virtual-kubelet"

	RoleLabelKey = "node-role.kubernetes.io/serverless"

	InstanceTypeLabelKey   = "node.kubernetes.io/instance-type"
	InstanceTypeLabelValue = "knaas.serverless"
)

// Capacity describes what a tenant may consume on this node. It mirrors the
// tenant's quota rather than any machine's size: reporting a fixed capacity
// moves an over-quota failure out of scheduling, where it surfaces as a clear
// Pending condition, into container creation, where it strands the pod.
type Capacity struct {
	CPU    string
	Memory string
	Pods   string
}

// Options describes a tenant's virtual node.
type Options struct {
	Name   string
	Tenant string
	Zone   string
	// Region is the OpenStack region this node's capsules are created in. It
	// comes from the credential, which resolves every service endpoint within
	// one region and cannot reach another.
	//
	// ⚠️ Load-bearing once a second region exists, and silent before then. An
	// availability zone name is only unique inside its region -- OVN treats a
	// zone as a string in ovn-cms-options on the chassis row, and every zone of
	// a region shares one northbound database. So two regions can both call a
	// zone "az1", and a volume provisioned in one would match the other's node
	// on zone alone. The failure is a claim that stays Bound and a pod that
	// stays Pending with neither object saying why -- the same shape §14 of the
	// volume reconciler already documents for the cross-zone case.
	Region   string
	Version  string
	OS       string
	Arch     string
	Capacity Capacity
	// InternalIP is the address the API server reaches this node's kubelet
	// endpoint on; without it logs and exec have no route back.
	InternalIP string
	// KubeletPort is the port the node's HTTP endpoint listens on.
	KubeletPort int32
}

// Build returns the Node object for a tenant's virtual node.
func Build(o Options) *corev1.Node {
	if o.OS == "" {
		o.OS = "linux"
	}
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	if o.KubeletPort == 0 {
		o.KubeletPort = 10250
	}

	labels := map[string]string{
		// The well-known trio is not optional: a great many charts carry
		// nodeSelector kubernetes.io/os=linux by default, and a node missing
		// it silently fails to match anything the tenant installs.
		corev1.LabelOSStable:   o.OS,
		corev1.LabelArchStable: o.Arch,
		corev1.LabelHostname:   o.Name,

		TypeLabelKey:         TypeLabelValue,
		RoleLabelKey:         "",
		InstanceTypeLabelKey: InstanceTypeLabelValue,
	}
	if o.Tenant != "" {
		labels[PoolLabelKey] = o.Tenant
	}
	if o.Region != "" {
		// Paired with the zone below, and the pairing is the point: a zone name
		// identifies a zone only within its region, so anything that matches on
		// zone alone matches across regions too.
		labels[corev1.LabelTopologyRegion] = o.Region
	}
	if o.Zone != "" {
		// A real zone, not decoration: capsules are placed in the matching
		// Zun availability zone, which makes topology spread and zone
		// anti-affinity mean what they say.
		labels[corev1.LabelTopologyZone] = o.Zone
	}

	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   o.Name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: o.OS,
				Architecture:    o.Arch,
				// Reported as a semver-compatible string: operators parse the
				// kubelet version to gate features, and a non-standard value
				// makes them misjudge or fail outright.
				KubeletVersion:          o.Version,
				ContainerRuntimeVersion: "zun://kata",
			},
			Capacity:    buildCapacity(o.Capacity),
			Allocatable: buildCapacity(o.Capacity),
			DaemonEndpoints: corev1.NodeDaemonEndpoints{
				KubeletEndpoint: corev1.DaemonEndpoint{Port: o.KubeletPort},
			},
			Conditions: []corev1.NodeCondition{NotReadyCondition("Registering the node")},
		},
	}

	if o.Tenant != "" {
		n.Spec.Taints = []corev1.Taint{{
			Key:    TenantTaintKey,
			Value:  o.Tenant,
			Effect: corev1.TaintEffectNoSchedule,
		}}
	}
	if o.InternalIP != "" {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: o.InternalIP},
		}
	}
	return n
}

func buildCapacity(c Capacity) corev1.ResourceList {
	if c.CPU == "" {
		c.CPU = "0"
	}
	if c.Memory == "" {
		c.Memory = "0"
	}
	if c.Pods == "" {
		c.Pods = "0"
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(c.CPU),
		corev1.ResourceMemory: resource.MustParse(c.Memory),
		corev1.ResourcePods:   resource.MustParse(c.Pods),
	}
}

// ReadyCondition reports a node whose control and data planes are both usable.
func ReadyCondition() corev1.NodeCondition {
	return condition(corev1.ConditionTrue, "KubeletReady", "zun is reachable")
}

// NotReadyCondition reports a node that cannot currently run pods. Unlike a
// physical node there is no machine to fail here: NotReady means this process
// or the Zun API behind it is unavailable, and the tenant should see that at
// the node level rather than as pods that never start.
func NotReadyCondition(msg string) corev1.NodeCondition {
	return condition(corev1.ConditionFalse, "KubeletNotReady", msg)
}

func condition(status corev1.ConditionStatus, reason, msg string) corev1.NodeCondition {
	now := metav1.NewTime(metav1.Now().Time)
	return corev1.NodeCondition{
		Type:               corev1.NodeReady,
		Status:             status,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            msg,
	}
}
