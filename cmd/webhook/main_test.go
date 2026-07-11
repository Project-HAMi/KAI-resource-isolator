/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testMountPath = "/usr/local/vgpu"

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func decodePatch(t *testing.T, raw []byte) []patchOp {
	t.Helper()
	var ops []patchOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	return ops
}

func opsForPath(ops []patchOp, path string) []patchOp {
	var out []patchOp
	for _, op := range ops {
		if op.Path == path {
			out = append(out, op)
		}
	}
	return out
}

func makePod(annotations map[string]string, containers, initContainers []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-pod",
			Namespace:   "test-ns",
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers:     containers,
			InitContainers: initContainers,
		},
	}
}

func TestPodNeedsInjection(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"nil annotations", nil, false},
		{"empty annotations", map[string]string{}, false},
		{"gpu-fraction", map[string]string{gpuFractionKey: "0.5"}, true},
		{"gpu-memory", map[string]string{gpuMemoryKey: "4096"}, true},
		{"both", map[string]string{gpuFractionKey: "0.5", gpuMemoryKey: "4096"}, true},
		{"unrelated annotation", map[string]string{"foo": "bar"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePod(tt.annotations, []corev1.Container{{Name: "main"}}, nil)
			if got := podNeedsInjection(pod); got != tt.want {
				t.Errorf("podNeedsInjection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasMount(t *testing.T) {
	c := &corev1.Container{
		Name: "main",
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeName, MountPath: testMountPath},
			{Name: volumeName, MountPath: "/etc/ld.so.preload", SubPath: "ld.so.preload"},
			{Name: "other", MountPath: "/data"},
		},
	}
	tests := []struct {
		name      string
		volName   string
		mountPath string
		subPath   string
		want      bool
	}{
		{"dir mount present", volumeName, testMountPath, "", true},
		{"preload mount present", volumeName, "/etc/ld.so.preload", "ld.so.preload", true},
		{"subPath mismatch", volumeName, "/etc/ld.so.preload", "", false},
		{"volume name mismatch", "other", testMountPath, "", false},
		{"path mismatch", volumeName, "/somewhere/else", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasMount(c, tt.volName, tt.mountPath, tt.subPath); got != tt.want {
				t.Errorf("hasMount(%q, %q, %q) = %v, want %v", tt.volName, tt.mountPath, tt.subPath, got, tt.want)
			}
		})
	}
}

func TestBuildJSONPatch_NoAnnotations(t *testing.T) {
	pod := makePod(nil, []corev1.Container{{Name: "main"}}, nil)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	if patch != nil {
		t.Errorf("expected nil patch for pod without gpu annotations, got %s", patch)
	}
}

func TestBuildJSONPatch_SingleContainer(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{Name: "main"}},
		nil,
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	volOps := opsForPath(ops, "/spec/volumes")
	if len(volOps) != 1 {
		t.Errorf("expected 1 volume op, got %d", len(volOps))
	}
	mountOps := opsForPath(ops, "/spec/containers/0/volumeMounts")
	if len(mountOps) != 1 {
		t.Fatalf("expected 1 combined mount op on container 0, got %d", len(mountOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("expected 2 mounts (dir + preload) in combined op, got %d", len(mounts))
	}
	if len(ops) != 2 {
		t.Errorf("expected 2 ops total, got %d: %s", len(ops), patch)
	}
}

// Preloading libvgpu.so is fatal in containers without GPU driver libraries,
// so only the fraction container may receive the mounts.
func TestBuildJSONPatch_MultiContainer_OnlyFractionContainer(t *testing.T) {
	pod := makePod(
		map[string]string{
			gpuFractionKey:                "0.5",
			"gpu-fraction-container-name": "gpu-container",
		},
		[]corev1.Container{
			{Name: "sidecar-a"},
			{Name: "gpu-container"},
			{Name: "sidecar-b"},
		},
		nil,
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	mountOps := opsForPath(ops, "/spec/containers/1/volumeMounts")
	if len(mountOps) != 1 {
		t.Fatalf("fraction container (index 1): expected 1 combined mount op, got %d", len(mountOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("fraction container (index 1): expected 2 mounts in combined op, got %d", len(mounts))
	}
	for _, i := range []int{0, 2} {
		path := fmt.Sprintf("/spec/containers/%d/volumeMounts", i)
		if mountOps := opsForPath(ops, path); len(mountOps) != 0 {
			t.Errorf("non-fraction container %d: expected 0 mount ops, got %d", i, len(mountOps))
		}
	}
}

func TestBuildJSONPatch_NoContainerNameAnnotation_DefaultsToFirstContainer(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{
			{Name: "main"},
			{Name: "sidecar"},
		},
		nil,
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	mountOps := opsForPath(ops, "/spec/containers/0/volumeMounts")
	if len(mountOps) != 1 {
		t.Fatalf("containers[0]: expected 1 combined mount op, got %d", len(mountOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("containers[0]: expected 2 mounts in combined op, got %d", len(mounts))
	}
	if mountOps := opsForPath(ops, "/spec/containers/1/volumeMounts"); len(mountOps) != 0 {
		t.Errorf("containers[1]: expected 0 mount ops, got %d", len(mountOps))
	}
}

func TestBuildJSONPatch_InitContainers_NotInjected(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{Name: "main"}},
		[]corev1.Container{{Name: "setup"}},
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	if initOps := opsForPath(ops, "/spec/initContainers/0/volumeMounts"); len(initOps) != 0 {
		t.Errorf("init container: expected 0 mount ops, got %d", len(initOps))
	}
	mainOps := opsForPath(ops, "/spec/containers/0/volumeMounts")
	if len(mainOps) != 1 {
		t.Fatalf("expected 1 combined mount op on container 0, got %d", len(mainOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mainOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("expected 2 mounts in combined op on container 0, got %d", len(mounts))
	}
}

func TestBuildJSONPatch_AnnotationNamesInitContainer(t *testing.T) {
	pod := makePod(
		map[string]string{
			gpuFractionKey:                "0.5",
			"gpu-fraction-container-name": "gpu-init",
		},
		[]corev1.Container{{Name: "main"}},
		[]corev1.Container{{Name: "gpu-init"}},
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	initOps := opsForPath(ops, "/spec/initContainers/0/volumeMounts")
	if len(initOps) != 1 {
		t.Fatalf("named init container: expected 1 combined mount op, got %d", len(initOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(initOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("named init container: expected 2 mounts in combined op, got %d", len(mounts))
	}
	if mainOps := opsForPath(ops, "/spec/containers/0/volumeMounts"); len(mainOps) != 0 {
		t.Errorf("regular container: expected 0 mount ops, got %d", len(mainOps))
	}
}

func TestBuildJSONPatch_AnnotationNamesUnknownContainer(t *testing.T) {
	pod := makePod(
		map[string]string{
			gpuFractionKey:                "0.5",
			"gpu-fraction-container-name": "no-such-container",
		},
		[]corev1.Container{{Name: "main"}},
		nil,
	)
	if _, err := buildJSONPatch(pod, testMountPath); err == nil {
		t.Error("expected error for unknown fraction container name, got nil")
	}
}

func TestBuildJSONPatch_VolumeAlreadyPresent(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{Name: "main"}},
		nil,
	)
	pod.Spec.Volumes = []corev1.Volume{{Name: volumeName}}

	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	if volOps := opsForPath(ops, "/spec/volumes/-"); len(volOps) != 0 {
		t.Errorf("expected no volume op when volume already present, got %d", len(volOps))
	}
	mountOps := opsForPath(ops, "/spec/containers/0/volumeMounts")
	if len(mountOps) != 1 {
		t.Fatalf("expected 1 combined mount op, got %d", len(mountOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("expected 2 mounts in combined op, got %d", len(mounts))
	}
}

func TestBuildJSONPatch_MountsAlreadyPresent(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{
			Name: "main",
			VolumeMounts: []corev1.VolumeMount{
				{Name: volumeName, MountPath: testMountPath},
				{Name: volumeName, MountPath: "/etc/ld.so.preload", SubPath: "ld.so.preload", ReadOnly: true},
			},
		}},
		nil,
	)
	pod.Spec.Volumes = []corev1.Volume{{Name: volumeName}}

	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	if patch != nil {
		t.Errorf("expected nil patch when everything already present, got %s", patch)
	}
}

func TestBuildJSONPatch_PartialMountsPresent(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{
			Name: "main",
			VolumeMounts: []corev1.VolumeMount{
				{Name: volumeName, MountPath: testMountPath},
			},
		}},
		nil,
	)
	pod.Spec.Volumes = []corev1.Volume{{Name: volumeName}}

	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	mountOps := opsForPath(ops, "/spec/containers/0/volumeMounts/-")
	if len(mountOps) != 1 {
		t.Fatalf("expected exactly 1 mount op (preload only), got %d", len(mountOps))
	}
	var mount corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mount); err != nil {
		t.Fatalf("failed to decode mount value: %v", err)
	}
	if mount.MountPath != "/etc/ld.so.preload" || mount.SubPath != "ld.so.preload" {
		t.Errorf("expected preload mount, got %+v", mount)
	}
}

func TestBuildJSONPatch_GpuMemoryAnnotation(t *testing.T) {
	pod := makePod(
		map[string]string{gpuMemoryKey: "4096"},
		[]corev1.Container{{Name: "main"}},
		nil,
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	if patch == nil {
		t.Fatal("expected patch for gpu-memory annotated pod, got nil")
	}
}

func TestBuildJSONPatch_MountValues(t *testing.T) {
	pod := makePod(
		map[string]string{gpuFractionKey: "0.5"},
		[]corev1.Container{{Name: "main"}},
		nil,
	)
	patch, err := buildJSONPatch(pod, testMountPath)
	if err != nil {
		t.Fatalf("buildJSONPatch() error = %v", err)
	}
	ops := decodePatch(t, patch)

	volOps := opsForPath(ops, "/spec/volumes")
	if len(volOps) != 1 {
		t.Fatalf("expected 1 volume op, got %d", len(volOps))
	}
	var vols []corev1.Volume
	if err := json.Unmarshal(volOps[0].Value, &vols); err != nil {
		t.Fatalf("failed to decode volumes value: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("expected 1 volume in combined op, got %d", len(vols))
	}
	vol := vols[0]
	if vol.Name != volumeName {
		t.Errorf("volume name = %q, want %q", vol.Name, volumeName)
	}
	if vol.HostPath == nil || vol.HostPath.Path != testMountPath {
		t.Errorf("volume hostPath = %+v, want path %q", vol.HostPath, testMountPath)
	}
	if vol.HostPath.Type == nil || *vol.HostPath.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("volume hostPath type = %v, want DirectoryOrCreate", vol.HostPath.Type)
	}

	mountOps := opsForPath(ops, "/spec/containers/0/volumeMounts")
	if len(mountOps) != 1 {
		t.Fatalf("expected 1 combined mount op, got %d", len(mountOps))
	}
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(mountOps[0].Value, &mounts); err != nil {
		t.Fatalf("failed to decode mounts value: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts in combined op, got %d", len(mounts))
	}
	dir, preload := mounts[0], mounts[1]
	if dir.MountPath != testMountPath || dir.ReadOnly {
		t.Errorf("dir mount = %+v, want mountPath %q readOnly=false", dir, testMountPath)
	}
	if preload.MountPath != "/etc/ld.so.preload" || preload.SubPath != "ld.so.preload" || !preload.ReadOnly {
		t.Errorf("preload mount = %+v, want /etc/ld.so.preload subPath=ld.so.preload readOnly=true", preload)
	}
}
