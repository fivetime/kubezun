package zun

import (
	corev1 "k8s.io/api/core/v1"
)

// A pod's securityContext travels on the container in the capsule spec, and
// Zun stores it beside the probes in the container's healthcheck column.
//
// ⚠️ Everything but privileged used to be dropped in silence. The capsule API
// had no securityContext on a container and the driver set only privileged, so
// a pod asking for a non-root user, a read-only root filesystem, dropped
// capabilities or a seccomp profile got none of them and nothing said so. A pod
// admitted under PodSecurity `restricted` is required to ask for all four, so
// this was every such pod — and the field that went missing was always the one
// that made it safer, which is the direction a silent failure must never take
// (template.go says the same about volumes).

// containerSecurity is what Zun is told about one container. The names are
// Kubernetes' own, so the two ends read the same.
type containerSecurity struct {
	RunAsUser  *int64 `json:"runAsUser,omitempty"`
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`
	// ReadOnlyRootFilesystem and AllowPrivilegeEscalation are pointers because
	// false is a request, not an absence: allowPrivilegeEscalation: false is
	// what PodSecurity restricted demands, and treating it as unset would drop
	// the very field the tenant was required to write.
	ReadOnlyRootFilesystem   *bool                  `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool                  `json:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *corev1.Capabilities   `json:"capabilities,omitempty"`
	SeccompProfile           *corev1.SeccompProfile `json:"seccompProfile,omitempty"`
}

// effectiveSecurity merges the pod's security context into the container's, the
// way Kubernetes does: a container's own setting wins, and what it leaves unset
// it inherits from the pod.
func effectiveSecurity(pod *corev1.Pod, c *corev1.Container) containerSecurity {
	var out containerSecurity

	if ps := pod.Spec.SecurityContext; ps != nil {
		out.RunAsUser = ps.RunAsUser
		out.RunAsGroup = ps.RunAsGroup
		out.SeccompProfile = ps.SeccompProfile
	}
	if cs := c.SecurityContext; cs != nil {
		if cs.RunAsUser != nil {
			out.RunAsUser = cs.RunAsUser
		}
		if cs.RunAsGroup != nil {
			out.RunAsGroup = cs.RunAsGroup
		}
		if cs.SeccompProfile != nil {
			out.SeccompProfile = cs.SeccompProfile
		}
		out.ReadOnlyRootFilesystem = cs.ReadOnlyRootFilesystem
		out.AllowPrivilegeEscalation = cs.AllowPrivilegeEscalation
		out.Capabilities = cs.Capabilities
	}
	return out
}

func (s containerSecurity) empty() bool {
	return s.RunAsUser == nil && s.RunAsGroup == nil &&
		s.ReadOnlyRootFilesystem == nil && s.AllowPrivilegeEscalation == nil &&
		s.Capabilities == nil && s.SeccompProfile == nil
}

// validateSecurity refuses what cannot be honoured, rather than accepting it and
// running with weaker isolation than was asked for.
func validateSecurity(pod *corev1.Pod) error {
	podNonRoot := false
	var podUser *int64
	if ps := pod.Spec.SecurityContext; ps != nil {
		podNonRoot = ps.RunAsNonRoot != nil && *ps.RunAsNonRoot
		podUser = ps.RunAsUser
	}

	for _, cs := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for i := range cs {
			c := &cs[i]
			nonRoot, user := podNonRoot, podUser
			if sc := c.SecurityContext; sc != nil {
				if sc.Privileged != nil && *sc.Privileged {
					return unsupported("securityContext.privileged",
						"the capsule API cannot express privileged containers")
				}
				if sc.RunAsNonRoot != nil {
					nonRoot = *sc.RunAsNonRoot
				}
				if sc.RunAsUser != nil {
					user = sc.RunAsUser
				}
			}

			// runAsNonRoot on its own is a check rather than a setting: the
			// kubelet reads the image's own user and refuses to start it if
			// that user is root. Nothing here can read the image, and the
			// runtime is only told a uid — so with no uid to send, the pod
			// would run as whatever the image says, which is the case this
			// asks to prevent.
			if nonRoot && user == nil {
				return unsupported("securityContext.runAsNonRoot without runAsUser",
					"honouring it means inspecting the image's own user, which "+
						"this cannot do; set runAsUser to the uid you want")
			}
			if user != nil && *user == 0 && nonRoot {
				return unsupported("securityContext.runAsUser: 0 with runAsNonRoot",
					"the two contradict each other")
			}
		}
	}
	return nil
}
