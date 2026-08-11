package zun

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
			EmptyDir *struct {
				Medium    string `json:"medium"`
				SizeLimit int64  `json:"sizeLimit"`
			} `json:"emptyDir"`
			Cinder *struct {
				VolumeID string `json:"volumeID"`
			} `json:"cinder"`
			NFS *struct {
				Export string `json:"export"`
			} `json:"nfs"`
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
		"downwardAPI": {DownwardAPI: &corev1.DownwardAPIVolumeSource{}},
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

// An emptyDir is the scratch space nearly every workload assumes: a sidecar
// writing where another reads, a cache directory, the writable path an image
// with a read-only root filesystem needs. Refusing it refused most charts.
func TestEmptyDirIsSharedByTheContainersThatMountIt(t *testing.T) {
	pod := podWithConfigVolume()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "scratch", MountPath: "/cache"})
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar", Image: "busybox",
		VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/shared"}},
	})

	got := render(t, pod, nil)

	var dirs int
	for _, v := range got.Spec.Volumes {
		if v.EmptyDir != nil {
			dirs++
			if v.Name != "scratch" {
				t.Errorf("emptyDir volume named %q", v.Name)
			}
		}
	}
	// One volume, however many containers mount it: Zun matches volumes to
	// mounts by name, so declaring it twice is two volumes claiming one name.
	if dirs != 1 {
		t.Fatalf("got %d emptyDir volumes, want 1: %+v", dirs, got.Spec.Volumes)
	}

	paths := map[string]string{}
	for _, c := range got.Spec.Containers {
		for _, m := range c.Mounts {
			if m.Name == "scratch" {
				paths[c.Name] = m.MountPath
			}
		}
	}
	if len(paths) != 2 {
		t.Fatalf("both containers should mount it, got %v", paths)
	}
	if paths["sidecar"] != "/shared" {
		t.Errorf("each container mounts it where it asked: %v", paths)
	}
}

// "Memory" is a tmpfs, and the size limit is real there because the kernel
// enforces it. Both have to reach the capsule or a caller asking for memory
// silently gets a directory on a disk.
func TestEmptyDirMediumAndSizeLimitAreCarried(t *testing.T) {
	limit := resource.MustParse("64Mi")
	pod := podWithConfigVolume()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "shm",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: &limit,
		}},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "shm", MountPath: "/dev/shm"})

	got := render(t, pod, nil)
	for _, v := range got.Spec.Volumes {
		if v.Name != "shm" {
			continue
		}
		if v.EmptyDir == nil {
			t.Fatal("no emptyDir on the volume")
		}
		if v.EmptyDir.Medium != "Memory" {
			t.Errorf("medium = %q, want Memory", v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit != 64*1024*1024 {
			t.Errorf("sizeLimit = %d bytes, want 67108864", v.EmptyDir.SizeLimit)
		}
		return
	}
	t.Fatal("the emptyDir volume was not emitted")
}

// A claim volume reaches the capsule as the storage behind it -- the Cinder
// volume or the NFS export -- never as the claim's name, which means nothing
// on the Zun side. A claim the provider failed to resolve fails the pod:
// starting without the volume leaves the application writing into its image.
func TestClaimVolumesAreEmittedAsTheirStorage(t *testing.T) {
	pod := podWithConfigVolume()
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "db-data"}}},
		corev1.Volume{Name: "media", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-media"}}},
	)
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "data", MountPath: "/var/lib/data"},
		corev1.VolumeMount{Name: "media", MountPath: "/media"},
	)

	tpl, err := BuildTemplate(pod, TemplateOptions{
		Files: map[string]map[string][]byte{"cfg": {"app.conf": []byte("x")}},
		Claims: map[string]ClaimMount{
			"data":  {Kind: "cinder", VolumeID: "vol-123"},
			"media": {Kind: "nfs", Export: "10.0.0.5:/shares/abc"},
		},
	})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var got renderedTemplate
	if err := json.Unmarshal(tpl, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var cinder, nfs bool
	for _, v := range got.Spec.Volumes {
		switch v.Name {
		case "data":
			if v.Cinder == nil || v.Cinder.VolumeID != "vol-123" {
				t.Errorf("data volume: %+v, want cinder vol-123", v)
			}
			cinder = true
		case "media":
			if v.NFS == nil || v.NFS.Export != "10.0.0.5:/shares/abc" {
				t.Errorf("media volume: %+v, want the export", v)
			}
			nfs = true
		}
	}
	if !cinder || !nfs {
		t.Fatalf("volumes missing: cinder=%v nfs=%v: %+v", cinder, nfs, got.Spec.Volumes)
	}

	// The same pod with the claims unresolved must fail, not start hollow.
	if _, err := BuildTemplate(pod, TemplateOptions{
		Files: map[string]map[string][]byte{"cfg": {"app.conf": []byte("x")}},
	}); err == nil {
		t.Fatal("an unresolved claim was accepted; the pod would run without its data")
	}
}
