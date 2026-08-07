package zun

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithConfigVolume(mounts ...corev1.VolumeMount) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "p", UID: "u1"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "cfg",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "app-cfg"},
					},
				},
			}},
			Containers: []corev1.Container{{
				Name: "app", Image: "nginx", VolumeMounts: mounts,
			}},
		},
	}
}

type renderedTemplate struct {
	Spec struct {
		Volumes []struct {
			Name string `json:"name"`
			File *struct {
				Contents string `json:"contents"`
			} `json:"file"`
		} `json:"volumes"`
		Containers []struct {
			Name   string `json:"name"`
			Mounts []struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			} `json:"volumeMounts"`
		} `json:"containers"`
	} `json:"spec"`
}

func render(t *testing.T, pod *corev1.Pod, files map[string]map[string][]byte) renderedTemplate {
	t.Helper()
	raw, err := BuildTemplate(pod, TemplateOptions{Files: files})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var out renderedTemplate
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// Kubernetes mounts a configMap as a directory of files; Zun's local volume
// driver writes one file per volume. Each key therefore becomes its own capsule
// volume mounted at its own full path, so the container sees the same directory.
func TestConfigMapVolumeBecomesOneCapsuleVolumePerKey(t *testing.T) {
	pod := podWithConfigVolume(corev1.VolumeMount{Name: "cfg", MountPath: "/etc/appcfg"})
	got := render(t, pod, map[string]map[string][]byte{
		"cfg": {"app.conf": []byte("key=value"), "extra.conf": []byte("a=b")},
	})

	if len(got.Spec.Volumes) != 2 {
		t.Fatalf("got %d capsule volumes, want one per key: %+v", len(got.Spec.Volumes), got.Spec.Volumes)
	}
	paths := map[string]string{}
	for _, m := range got.Spec.Containers[0].Mounts {
		paths[m.Name] = m.MountPath
	}
	if paths["cfg-app.conf"] != "/etc/appcfg/app.conf" {
		t.Errorf("mount paths = %v, want each key under the mount path", paths)
	}

	// Base64 so a value with newlines, or one that is not text, survives.
	for _, v := range got.Spec.Volumes {
		if v.Name != "cfg-app.conf" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(v.File.Contents)
		if err != nil || string(data) != "key=value" {
			t.Errorf("contents = %q (%v), want key=value", data, err)
		}
	}
}

// With a subPath the mount path is the file itself and the rest of the volume
// is not mounted — mounting the whole volume there would bury the directory the
// pod meant to add one file to.
func TestSubPathMountsOneKeyAtTheMountPath(t *testing.T) {
	pod := podWithConfigVolume(corev1.VolumeMount{
		Name: "cfg", MountPath: "/etc/nginx/nginx.conf", SubPath: "app.conf",
	})
	got := render(t, pod, map[string]map[string][]byte{
		"cfg": {"app.conf": []byte("key=value"), "unrelated.conf": []byte("a=b")},
	})

	if len(got.Spec.Volumes) != 1 {
		t.Fatalf("got %d volumes, want only the named key: %+v", len(got.Spec.Volumes), got.Spec.Volumes)
	}
	if m := got.Spec.Containers[0].Mounts; len(m) != 1 || m[0].MountPath != "/etc/nginx/nginx.conf" {
		t.Errorf("mounts = %+v, want the file at the mount path itself", m)
	}
}

// A volume kind that cannot be carried into a capsule has to be refused. Left
// to fall through, the pod would run and report Ready with the file its
// application reads simply absent.
func TestUnsupportedVolumesAreRefusedNotDropped(t *testing.T) {
	for name, source := range map[string]corev1.VolumeSource{
		"emptyDir":    {EmptyDir: &corev1.EmptyDirVolumeSource{}},
		"downwardAPI": {DownwardAPI: &corev1.DownwardAPIVolumeSource{}},
		"pvc": {PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: "data"}},
	} {
		pod := podWithConfigVolume()
		pod.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: source}}
		if _, err := BuildTemplate(pod, TemplateOptions{}); err == nil {
			t.Errorf("%s volume was accepted and would have been silently dropped", name)
		}
	}
}

// Two containers mounting the same volume must not each declare it: Zun matches
// volumes to mounts by name, and a duplicate name is two volumes claiming one.
func TestSharedVolumeIsDeclaredOnce(t *testing.T) {
	pod := podWithConfigVolume(corev1.VolumeMount{Name: "cfg", MountPath: "/etc/appcfg"})
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar", Image: "busybox",
		VolumeMounts: []corev1.VolumeMount{{Name: "cfg", MountPath: "/config"}},
	})
	got := render(t, pod, map[string]map[string][]byte{"cfg": {"app.conf": []byte("x")}})

	if len(got.Spec.Volumes) != 1 {
		t.Errorf("got %d volumes for one shared key: %+v", len(got.Spec.Volumes), got.Spec.Volumes)
	}
	// Both containers still mount it, each at its own path.
	for _, c := range got.Spec.Containers {
		if len(c.Mounts) != 1 {
			t.Errorf("container %s has %d mounts, want 1", c.Name, len(c.Mounts))
		}
	}
}

func TestVolumeNameSanitisesKeys(t *testing.T) {
	// Kubernetes allows key names Zun's volume pattern does not.
	got, err := volumeName("cfg", "nested/path.conf")
	if err != nil {
		t.Fatalf("volumeName: %v", err)
	}
	if got != "cfg-nested-path.conf" {
		t.Errorf("volumeName = %q, want the slash replaced", got)
	}
	if _, err := volumeName("", ""); err == nil {
		t.Error("an empty name was accepted; Zun requires at least two characters")
	}
}
