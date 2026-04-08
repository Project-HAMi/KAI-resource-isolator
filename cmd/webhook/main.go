/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultListen       = ":8443"
	volumeName          = "kai-resource-isolator-vgpu"
	injectAnnotationKey = "kai-resource-isolator.io/inject"
)

func main() {
	certFile := flag.String("tls-cert-file", "/etc/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("tls-private-key-file", "/etc/tls/tls.key", "TLS private key")
	listen := flag.String("listen", defaultListen, "Listen address")
	containerMount := flag.String("container-vgpu-mount", getenv("CONTAINER_VGPU_MOUNT", "/usr/local/vgpu"), "Mount path inside the pod for the node vgpu directory (must match DaemonSet install path and ld.so.preload)")
	resources := flag.String("gpu-resources", getenv("GPU_SHARE_RESOURCES", "nvidia.com/gpu,nvidia.com/gpumem,nvidia.com/gpucores"), "Comma-separated resource names that identify GPU-sharing workloads")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		handleMutate(w, r, strings.Split(*resources, ","), *containerMount)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

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

func handleMutate(w http.ResponseWriter, r *http.Request, resourceKeys []string, containerMount string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "missing request", http.StatusBadRequest)
		return
	}

	resp := admissionv1.AdmissionResponse{
		UID:     review.Request.UID,
		Allowed: true,
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err != nil {
		resp.Result = &metav1.Status{
			Message: fmt.Sprintf("unmarshal pod: %v", err),
			Code:    http.StatusBadRequest,
		}
		resp.Allowed = false
		writeAdmission(w, &review, resp)
		return
	}

	if pod.Annotations != nil && strings.EqualFold(pod.Annotations[injectAnnotationKey], "false") {
		writeAdmission(w, &review, resp)
		return
	}

	patch, err := buildJSONPatch(&pod, resourceKeys, containerMount)
	if err != nil {
		resp.Result = &metav1.Status{Message: err.Error(), Code: http.StatusInternalServerError}
		resp.Allowed = false
		writeAdmission(w, &review, resp)
		return
	}
	if len(patch) == 0 {
		writeAdmission(w, &review, resp)
		return
	}
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

func buildJSONPatch(pod *corev1.Pod, resourceKeys []string, containerMount string) ([]byte, error) {
	keys := normalizeKeys(resourceKeys)
	if !podNeedsInjection(pod, keys) {
		return nil, nil
	}

	var ops []map[string]interface{}

	hasVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			hasVol = true
			break
		}
	}
	if !hasVol {
		vol := map[string]interface{}{
			"name": volumeName,
			"hostPath": map[string]interface{}{
				"path": containerMount,
				"type": string(corev1.HostPathDirectoryOrCreate),
			},
		}
		ops = append(ops, map[string]interface{}{
			"op":    "add",
			"path":  "/spec/volumes/-",
			"value": vol,
		})
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

	for i := range pod.Spec.InitContainers {
		if !containerUsesGPUShare(&pod.Spec.InitContainers[i], keys) {
			continue
		}
		c := &pod.Spec.InitContainers[i]
		if !hasMount(c, volumeName, containerMount, "") {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/initContainers/%d/volumeMounts/-", i),
				"value": mountDir,
			})
		}
		if !hasMount(c, volumeName, "/etc/ld.so.preload", "ld.so.preload") {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/initContainers/%d/volumeMounts/-", i),
				"value": mountPreload,
			})
		}
	}
	for i := range pod.Spec.Containers {
		if !containerUsesGPUShare(&pod.Spec.Containers[i], keys) {
			continue
		}
		c := &pod.Spec.Containers[i]
		if !hasMount(c, volumeName, containerMount, "") {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/containers/%d/volumeMounts/-", i),
				"value": mountDir,
			})
		}
		if !hasMount(c, volumeName, "/etc/ld.so.preload", "ld.so.preload") {
			ops = append(ops, map[string]interface{}{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/containers/%d/volumeMounts/-", i),
				"value": mountPreload,
			})
		}
	}

	if len(ops) == 0 {
		return nil, nil
	}
	return json.Marshal(ops)
}

func normalizeKeys(keys []string) []string {
	var out []string
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func podNeedsInjection(pod *corev1.Pod, keys []string) bool {
	for i := range pod.Spec.InitContainers {
		if containerUsesGPUShare(&pod.Spec.InitContainers[i], keys) {
			return true
		}
	}
	for i := range pod.Spec.Containers {
		if containerUsesGPUShare(&pod.Spec.Containers[i], keys) {
			return true
		}
	}
	return false
}

func containerUsesGPUShare(c *corev1.Container, keys []string) bool {
	for _, k := range keys {
		rn := corev1.ResourceName(k)
		if q, ok := c.Resources.Requests[rn]; ok && !q.IsZero() {
			return true
		}
		if q, ok := c.Resources.Limits[rn]; ok && !q.IsZero() {
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
