/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements the mutating admission webhook for KAI resource isolator.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultListen       = ":8443"
	volumeName          = "kai-resource-isolator-vgpu"
	injectAnnotationKey = "kai-resource-isolator.io/inject"
	gpuFractionKey      = "gpu-fraction"
	gpuMemoryKey        = "gpu-memory"
	// cudaMemLimitEnv is the HAMi-core (libvgpu) memory-limit env var. KAI sets
	// the gpu-memory annotation (MiB) but does not pass this env, so libvgpu would
	// not enforce any cap. We translate gpu-memory -> CUDA_DEVICE_MEMORY_LIMIT=<v>m.
	cudaMemLimitEnv = "CUDA_DEVICE_MEMORY_LIMIT"
)

// perGpuVramMiB is the per-GPU VRAM (MiB) used to translate a gpu-fraction into an
// absolute HAMi-core memory cap. It comes from the PER_GPU_VRAM_MIB env
// (authoritative) or is autodetected from node labels and refreshed in the
// background, so access is atomic. Zero means "unknown" — gpu-fraction caps are
// then skipped rather than guessed (gpu-memory pods are unaffected).
var perGpuVramMiB atomic.Int64

func main() {
	certFile := flag.String("tls-cert-file", "/etc/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("tls-private-key-file", "/etc/tls/tls.key", "TLS private key")
	listen := flag.String("listen", defaultListen, "Listen address")
	containerMount := flag.String("container-vgpu-mount", getenv("CONTAINER_VGPU_MOUNT", "/usr/local/vgpu"), "Mount path inside the pod for the node vgpu directory (must match DaemonSet install path and ld.so.preload)")
	flag.Parse()

	// Per-GPU VRAM basis for translating gpu-fraction into a HAMi-core memory cap.
	// Precedence: explicit PER_GPU_VRAM_MIB env (authoritative) > autodetect from
	// node labels. No hardcoded default — unknown basis means gpu-fraction caps are
	// skipped (see buildJSONPatch).
	envOverride := false
	if v := os.Getenv("PER_GPU_VRAM_MIB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			perGpuVramMiB.Store(n)
			envOverride = true
			log.Printf("per-GPU VRAM basis = %d MiB (from PER_GPU_VRAM_MIB)", n)
		} else {
			log.Printf("ignoring invalid PER_GPU_VRAM_MIB=%q", v)
		}
	}
	if !envOverride {
		if cs, err := newInClusterClientset(); err != nil {
			log.Printf("per-GPU VRAM autodetect unavailable (%v); set PER_GPU_VRAM_MIB to enable gpu-fraction caps", err)
		} else {
			startVramAutodetect(context.Background(), cs)
		}
	}

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

	// Translate the KAI resource share into the HAMi-core memory-limit env so
	// libvgpu actually enforces the per-pod VRAM cap. KAI sets the annotation but
	// passes no such env. gpu-memory carries an absolute MiB; gpu-fraction carries
	// a share that we multiply by the per-GPU VRAM. The two are mutually exclusive.
	limitValue := ""
	if memMiB, ok := pod.Annotations[gpuMemoryKey]; ok && memMiB != "" {
		limitValue = memMiB + "m"
	} else if fracStr, ok := pod.Annotations[gpuFractionKey]; ok && fracStr != "" {
		basis := perGpuVramMiB.Load()
		if frac, err := strconv.ParseFloat(fracStr, 64); err != nil || frac <= 0 {
			log.Printf("gpu-fraction %q unparseable; skipping memory-limit injection", fracStr)
		} else if basis <= 0 {
			log.Printf("per-GPU VRAM unknown (no PER_GPU_VRAM_MIB env and no %s node label); skipping gpu-fraction memory cap", gpuMemoryNodeLabel)
		} else if limitMiB := int64(frac * float64(basis)); limitMiB > 0 {
			limitValue = strconv.FormatInt(limitMiB, 10) + "m"
		}
	}
	if limitValue != "" {
		for i := range pod.Spec.InitContainers {
			ops = appendMemLimitEnvOp(ops, &pod.Spec.InitContainers[i], "initContainers", i, limitValue)
		}
		for i := range pod.Spec.Containers {
			ops = appendMemLimitEnvOp(ops, &pod.Spec.Containers[i], "containers", i, limitValue)
		}
	}

	if len(ops) == 0 {
		return nil, nil
	}
	return json.Marshal(ops)
}

// appendMemLimitEnvOp adds a JSON patch op setting CUDA_DEVICE_MEMORY_LIMIT on the
// container, unless it is already present (user-set values win). It handles the
// case where the container has no env array yet (add the array, not append to it).
func appendMemLimitEnvOp(ops []map[string]interface{}, c *corev1.Container, field string, i int, limitValue string) []map[string]interface{} {
	for _, e := range c.Env {
		if e.Name == cudaMemLimitEnv {
			return ops
		}
	}
	envVar := map[string]interface{}{"name": cudaMemLimitEnv, "value": limitValue}
	if len(c.Env) == 0 {
		return append(ops, map[string]interface{}{
			"op":    "add",
			"path":  fmt.Sprintf("/spec/%s/%d/env", field, i),
			"value": []map[string]interface{}{envVar},
		})
	}
	return append(ops, map[string]interface{}{
		"op":    "add",
		"path":  fmt.Sprintf("/spec/%s/%d/env/-", field, i),
		"value": envVar,
	})
}

func podNeedsInjection(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	_, hasFraction := pod.Annotations[gpuFractionKey]
	_, hasMemory := pod.Annotations[gpuMemoryKey]
	return hasFraction || hasMemory
}

func hasMount(c *corev1.Container, volName, mountPath, subPath string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == volName && m.MountPath == mountPath && m.SubPath == subPath {
			return true
		}
	}
	return false
}
