/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements the mutating admission webhook for KAI resource isolator.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultListen               = ":8443"
	volumeName                  = "kai-resource-isolator-vgpu"
	volumeNameContainers        = "kai-resource-isolator-containers"
	volumeNameVgpulock          = "kai-resource-isolator-vgpulock"
	injectAnnotationKey         = "kai-resource-isolator.io/inject"
	gpuFractionKey              = "gpu-fraction"
	gpuMemoryKey                = "gpu-memory"
	gpuFractionContainerNameKey = "gpu-fraction-container-name" // KAI-scheduler annotation
	envPodUID                   = "POD_UID"
	envContainerName            = "CONTAINER_NAME"
	envContainerVgpuMount       = "CONTAINER_VGPU_MOUNT"
	vgpuLockPath                = "/tmp/vgpulock"
)

func main() {
	certFile := flag.String("tls-cert-file", "/etc/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("tls-private-key-file", "/etc/tls/tls.key", "TLS private key")
	listen := flag.String("listen", defaultListen, "Listen address")
	containerMount := flag.String("container-vgpu-mount", getenv("CONTAINER_VGPU_MOUNT", "/usr/local/vgpu"), "Mount path inside the pod for the node vgpu directory (must match DaemonSet install path and ld.so.preload)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		handleMutate(w, r, *containerMount)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("webhook starting listen=%s containerVgpuMount=%s annotationKeys=%s|%s", *listen, *containerMount, gpuFractionKey, gpuMemoryKey)

	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handleMutate(w http.ResponseWriter, r *http.Request, containerMount string) {
	if r.Method != http.MethodPost {
		log.Printf("mutate reject: method=%s", r.Method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("mutate read body failed: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		log.Printf("mutate decode admission review failed: %v", err)
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		log.Printf("mutate reject: missing request")
		http.Error(w, "missing request", http.StatusBadRequest)
		return
	}

	resp := admissionv1.AdmissionResponse{
		UID:     review.Request.UID,
		Allowed: true,
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err != nil {
		log.Printf("mutate uid=%s unmarshal pod failed: %v", review.Request.UID, err)
		resp.Result = &metav1.Status{
			Message: fmt.Sprintf("unmarshal pod: %v", err),
			Code:    http.StatusBadRequest,
		}
		resp.Allowed = false
		writeAdmission(w, &review, resp)
		return
	}

	if pod.Annotations != nil && strings.EqualFold(pod.Annotations[injectAnnotationKey], "false") {
		log.Printf("mutate uid=%s ns=%s pod=%s skipped: annotation %s=false", review.Request.UID, pod.Namespace, pod.Name, injectAnnotationKey)
		writeAdmission(w, &review, resp)
		return
	}

	patch, err := buildJSONPatch(&pod, containerMount)
	if err != nil {
		log.Printf("mutate uid=%s ns=%s pod=%s build patch failed: %v", review.Request.UID, pod.Namespace, pod.Name, err)
		resp.Result = &metav1.Status{Message: err.Error(), Code: http.StatusInternalServerError}
		resp.Allowed = false
		writeAdmission(w, &review, resp)
		return
	}
	if len(patch) == 0 {
		log.Printf("mutate uid=%s ns=%s pod=%s skipped: missing annotations %q or %q", review.Request.UID, pod.Namespace, pod.Name, gpuFractionKey, gpuMemoryKey)
		writeAdmission(w, &review, resp)
		return
	}
	log.Printf("mutate uid=%s ns=%s pod=%s injected: patchBytes=%d", review.Request.UID, pod.Namespace, pod.Name, len(patch))
	pt := admissionv1.PatchTypeJSONPatch
	resp.Patch = patch
	resp.PatchType = &pt

	writeAdmission(w, &review, resp)
}

func writeAdmission(w http.ResponseWriter, review *admissionv1.AdmissionReview, resp admissionv1.AdmissionResponse) {
	review.Response = &resp
	if review.APIVersion == "" {
		review.APIVersion = admissionv1.SchemeGroupVersion.String()
	}
	if review.Kind == "" {
		review.Kind = "AdmissionReview"
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(review); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func buildJSONPatch(pod *corev1.Pod, containerMount string) ([]byte, error) {
	if !podNeedsInjection(pod) {
		return nil, nil
	}

	target, specField, err := fractionContainer(pod)
	if err != nil {
		return nil, err
	}
	// v1 metrics mounts/env apply only when the fraction target is a regular container.
	metricsEnabled := specField == "containers"
	containersMountPath := path.Join(containerMount, "containers")

	var ops []map[string]interface{}

	var newVols []interface{}
	if !hasVolume(pod, volumeName) {
		newVols = append(newVols, hostPathVolume(volumeName, containerMount))
	}
	if metricsEnabled {
		if !hasVolume(pod, volumeNameContainers) {
			newVols = append(newVols, hostPathVolume(volumeNameContainers, containersMountPath))
		}
		if !hasVolume(pod, volumeNameVgpulock) {
			newVols = append(newVols, hostPathVolume(volumeNameVgpulock, vgpuLockPath))
		}
	}
	if len(newVols) > 0 {
		if len(pod.Spec.Volumes) == 0 {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  "/spec/volumes",
				"value": newVols,
			})
		} else {
			for _, vol := range newVols {
				ops = append(ops, map[string]interface{}{
					"op":    "add",
					"path":  "/spec/volumes/-",
					"value": vol,
				})
			}
		}
	}

	mountDir := map[string]interface{}{
		"name":      volumeName,
		"mountPath": containerMount,
		"readOnly":  false,
	}
	mountPreload := map[string]interface{}{
		"name":      volumeName,
		"mountPath": "/etc/ld.so.preload",
		"subPath":   "ld.so.preload",
		"readOnly":  true,
	}
	mountContainers := map[string]interface{}{
		"name":      volumeNameContainers,
		"mountPath": containersMountPath,
		"readOnly":  false,
	}
	mountVgpulock := map[string]interface{}{
		"name":      volumeNameVgpulock,
		"mountPath": vgpuLockPath,
		"readOnly":  false,
	}

	c := target.container
	var newMounts []interface{}
	if !hasMount(c, volumeName, containerMount, "") {
		newMounts = append(newMounts, mountDir)
	}
	if !hasMount(c, volumeName, "/etc/ld.so.preload", "ld.so.preload") {
		newMounts = append(newMounts, mountPreload)
	}
	if metricsEnabled {
		if !hasMount(c, volumeNameContainers, containersMountPath, "") {
			newMounts = append(newMounts, mountContainers)
		}
		if !hasMount(c, volumeNameVgpulock, vgpuLockPath, "") {
			newMounts = append(newMounts, mountVgpulock)
		}
	}
	if len(newMounts) > 0 {
		if len(c.VolumeMounts) == 0 {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/%s/%d/volumeMounts", specField, target.index),
				"value": newMounts,
			})
		} else {
			for _, m := range newMounts {
				ops = append(ops, map[string]interface{}{
					"op":    "add",
					"path":  fmt.Sprintf("/spec/%s/%d/volumeMounts/-", specField, target.index),
					"value": m,
				})
			}
		}
	}

	if metricsEnabled {
		ops = appendMetricsEnvIfMissing(ops, specField, target.index, c, c.Name, containerMount)
	}

	if len(ops) == 0 {
		return nil, nil
	}
	return json.Marshal(ops)
}

type containerRef struct {
	container *corev1.Container
	index     int
}

// fractionContainer resolves the GPU fraction container — the only safe
// preload target — mirroring KAI-scheduler's GetFractionContainerRef:
// named by annotation (init containers first), defaulting to containers[0].
func fractionContainer(pod *corev1.Pod) (ref containerRef, specField string, err error) {
	name, found := "", false
	if pod.Annotations != nil {
		name, found = pod.Annotations[gpuFractionContainerNameKey]
	}
	if !found {
		if len(pod.Spec.Containers) == 0 {
			return containerRef{}, "", fmt.Errorf("pod has no containers")
		}
		return containerRef{container: &pod.Spec.Containers[0], index: 0}, "containers", nil
	}

	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return containerRef{container: &pod.Spec.InitContainers[i], index: i}, "initContainers", nil
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return containerRef{container: &pod.Spec.Containers[i], index: i}, "containers", nil
		}
	}
	return containerRef{}, "", fmt.Errorf("container with name %s not found for fraction request", name)
}

func hostPathVolume(name, hostPath string) map[string]interface{} {
	return map[string]interface{}{
		"name": name,
		"hostPath": map[string]interface{}{
			"path": hostPath,
			"type": string(corev1.HostPathDirectoryOrCreate),
		},
	}
}

func appendMetricsEnvIfMissing(ops []map[string]interface{}, containerField string, index int, c *corev1.Container, containerName, containerMount string) []map[string]interface{} {
	basePath := fmt.Sprintf("/spec/%s/%d/env", containerField, index)

	var newEnvs []interface{}
	if !hasEnvVar(c, envPodUID) {
		newEnvs = append(newEnvs, map[string]interface{}{
			"name": envPodUID,
			"valueFrom": map[string]interface{}{
				"fieldRef": map[string]interface{}{
					"apiVersion": "v1",
					"fieldPath":  "metadata.uid",
				},
			},
		})
	}
	if !hasEnvVar(c, envContainerName) {
		newEnvs = append(newEnvs, map[string]interface{}{
			"name":  envContainerName,
			"value": containerName,
		})
	}
	if !hasEnvVar(c, envContainerVgpuMount) {
		newEnvs = append(newEnvs, map[string]interface{}{
			"name":  envContainerVgpuMount,
			"value": containerMount,
		})
	}
	if len(newEnvs) == 0 {
		return ops
	}
	if c.Env == nil {
		return append(ops, map[string]interface{}{
			"op":    "add",
			"path":  basePath,
			"value": newEnvs,
		})
	}
	for _, env := range newEnvs {
		ops = append(ops, map[string]interface{}{
			"op":    "add",
			"path":  basePath + "/-",
			"value": env,
		})
	}
	return ops
}

func podNeedsInjection(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	_, hasFraction := pod.Annotations[gpuFractionKey]
	_, hasMemory := pod.Annotations[gpuMemoryKey]
	return hasFraction || hasMemory
}

func hasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func hasMount(c *corev1.Container, volName, mountPath, subPath string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == volName && m.MountPath == mountPath && m.SubPath == subPath {
			return true
		}
	}
	return false
}

func hasEnvVar(c *corev1.Container, name string) bool {
	for _, e := range c.Env {
		if e.Name == name {
			return true
		}
	}
	return false
}
