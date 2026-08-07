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
func buildFileVolumes(pod *corev1.Pod, files map[string]map[string][]byte) (
	[]capsuleVolume, map[string][]volumeMount, error) {

	var volumes []capsuleVolume
	mounts := make(map[string][]volumeMount)
	// A capsule volume name is matched by string, so each has to be unique
	// across the whole capsule even when two containers mount the same volume.
	emitted := make(map[string]bool)

	for _, c := range allContainers(pod) {
		for _, m := range c.VolumeMounts {
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

func allContainers(pod *corev1.Pod) []corev1.Container {
	out := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	out = append(out, pod.Spec.InitContainers...)
	return append(out, pod.Spec.Containers...)
}
