package zun

import (
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// buildFileVolumes turns a pod's configMap and secret volumes into capsule
// volumes, and returns the mounts each container needs.
//
// Kubernetes mounts such a volume as a directory whose files are the keys.
// Zun's local volume driver writes one file per volume, so a configMap with
// three keys becomes three capsule volumes, each mounted at its own full path.
// The container sees the same directory either way.
func buildFileVolumes(pod *corev1.Pod, files map[string]map[string][]byte,
	claims map[string]ClaimMount) ([]capsuleVolume, map[string][]volumeMount, error) {

	var volumes []capsuleVolume
	mounts := make(map[string][]volumeMount)

	// The pod's emptyDir and claim volumes, by name: they are mounted as
	// directories, not rendered as files, so they are looked up separately
	// below.
	dirs := make(map[string]*corev1.EmptyDirVolumeSource)
	claimOf := make(map[string]string)
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.EmptyDir != nil {
			dirs[v.Name] = v.EmptyDir
		}
		if v.PersistentVolumeClaim != nil {
			claimOf[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	// A capsule volume name is matched by string, so each has to be unique
	// across the whole capsule even when two containers mount the same volume.
	emitted := make(map[string]bool)

	for _, c := range allContainers(pod) {
		for _, m := range c.VolumeMounts {
			if claim, ok := claimOf[m.Name]; ok {
				resolved, have := claims[m.Name]
				if !have {
					// The provider resolves every claim before building the
					// template, so reaching here is a bug there -- but failing
					// the pod is still right: starting it without the volume
					// leaves the application writing into its own image.
					return nil, nil, fmt.Errorf(
						"volume %s: claim %s was not resolved", m.Name, claim)
				}
				if !emitted[m.Name] {
					vol := capsuleVolume{Name: m.Name}
					switch resolved.Kind {
					case "cinder":
						vol.Cinder = &cinderData{VolumeID: resolved.VolumeID,
							FSGroup: fsGroupOf(pod)}
					case "nfs":
						vol.NFS = &nfsData{Export: resolved.Export,
							ShareID: resolved.VolumeID, FSGroup: fsGroupOf(pod)}
					default:
						return nil, nil, fmt.Errorf(
							"volume %s: claim %s resolves to unknown storage %q",
							m.Name, claim, resolved.Kind)
					}
					volumes = append(volumes, vol)
					emitted[m.Name] = true
				}
				mounts[c.Name] = append(mounts[c.Name], volumeMount{
					Name: m.Name, MountPath: m.MountPath,
				})
				continue
			}
			if src, ok := dirs[m.Name]; ok {
				// One capsule volume per pod volume, mounted at the path each
				// container asked for. Every container that names it gets the
				// same directory, which is the whole point of an emptyDir: a
				// sidecar writes where another reads.
				if !emitted[m.Name] {
					volumes = append(volumes, capsuleVolume{
						Name: m.Name, EmptyDir: emptyDirOf(src),
					})
					emitted[m.Name] = true
				}
				mounts[c.Name] = append(mounts[c.Name], volumeMount{
					Name: m.Name, MountPath: m.MountPath,
				})
				continue
			}
			content, ok := files[m.Name]
			if !ok {
				// Either a volume kind Validate refused, or one whose content
				// could not be read. Nothing here can mount it, and letting the
				// container start without it hides the failure inside the
				// application.
				continue
			}
			if m.SubPathExpr != "" {
				return nil, nil, unsupported("volumeMounts[].subPathExpr",
					"expanding a subPath needs the pod's environment resolved on the node")
			}

			for name, data := range content {
				if m.SubPath != "" && m.SubPath != name {
					// A subPath mount names one key; the rest of the volume is
					// not mounted at all.
					continue
				}
				target := path.Join(m.MountPath, name)
				if m.SubPath != "" {
					// With a subPath the mount path is the file itself, not the
					// directory holding it.
					target = m.MountPath
				}

				volName, err := volumeName(m.Name, name)
				if err != nil {
					return nil, nil, err
				}
				if !emitted[volName] {
					volumes = append(volumes, capsuleVolume{
						Name: volName,
						File: &fileData{Contents: base64.StdEncoding.EncodeToString(data)},
					})
					emitted[volName] = true
				}
				mounts[c.Name] = append(mounts[c.Name], volumeMount{
					Name: volName, MountPath: target,
				})
			}
		}
	}
	return volumes, mounts, nil
}

// zunVolumeName is what Zun accepts for a volume name.
var zunVolumeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// volumeName derives a capsule volume name for one file of one volume.
//
// Kubernetes allows key names Zun's volume name pattern does not, so anything
// outside it is replaced. The volume name is only ever matched against the
// mount this function emits alongside it, so the substitution cannot collide
// with anything a tenant chose.
func volumeName(volume, key string) (string, error) {
	name := zunVolumeName.ReplaceAllString(volume+"-"+key, "-")
	name = strings.TrimLeft(name, "-._")
	if len(name) < 2 {
		return "", fmt.Errorf("volume %q key %q: no usable capsule volume name", volume, key)
	}
	if len(name) > 255 {
		return "", fmt.Errorf(
			"volume %q key %q: name is longer than the 255 characters Zun accepts", volume, key)
	}
	return name, nil
}

// fsGroupOf is the pod's declared fsGroup, or zero when it has none.
func fsGroupOf(pod *corev1.Pod) int64 {
	if sc := pod.Spec.SecurityContext; sc != nil && sc.FSGroup != nil {
		return *sc.FSGroup
	}
	return 0
}

// emptyDirOf translates the pod's emptyDir into the capsule's.
//
// A size limit is passed on in bytes. Kubernetes enforces it on a tmpfs and,
// on a node directory, only by evicting the pod later -- a capsule has no
// eviction, so there it is carried for the record and not pretended about.
func emptyDirOf(src *corev1.EmptyDirVolumeSource) *emptyDirData {
	out := &emptyDirData{Medium: string(src.Medium)}
	if src.SizeLimit != nil {
		out.SizeLimit = src.SizeLimit.Value()
	}
	return out
}

func allContainers(pod *corev1.Pod) []corev1.Container {
	out := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	out = append(out, pod.Spec.InitContainers...)
	return append(out, pod.Spec.Containers...)
}
