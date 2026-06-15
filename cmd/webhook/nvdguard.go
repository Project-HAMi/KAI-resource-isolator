/*
Copyright The HAMi Authors.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const nvidiaVisibleDevicesEnv = "NVIDIA_VISIBLE_DEVICES"

// guardMode controls the NVIDIA_VISIBLE_DEVICES guard: "off" | "audit" | "enforce".
// Default "audit" (log only, no mutation) so a misconfigured allowlist can never
// break workloads before the logs are observed. Set via NVIDIA_VISIBLE_DEVICES_GUARD.
var guardMode = "audit"

// guardAllowedNamespaces: namespaces whose GPU-runtime pods are always trusted —
// system/infra components legitimately use runtimeClass=nvidia without the KAI
// sharing annotations. Override via GUARD_ALLOWED_NAMESPACES (comma-separated).
var guardAllowedNamespaces = map[string]bool{
	"gpu-operator":             true,
	"kube-system":              true,
	"kai-scheduler":            true,
	"kai-resource-reservation": true,
	"nvidia-network-operator":  true,
}

// initNvdGuard loads guard config from env (called once from main).
func initNvdGuard() {
	if m := strings.ToLower(strings.TrimSpace(os.Getenv("NVIDIA_VISIBLE_DEVICES_GUARD"))); m != "" {
		switch m {
		case "off", "audit", "enforce":
			guardMode = m
		default:
			log.Printf("invalid NVIDIA_VISIBLE_DEVICES_GUARD=%q, keeping %q", m, guardMode)
		}
	}
	if ns := strings.TrimSpace(os.Getenv("GUARD_ALLOWED_NAMESPACES")); ns != "" {
		guardAllowedNamespaces = map[string]bool{}
		for _, n := range strings.Split(ns, ",") {
			if n = strings.TrimSpace(n); n != "" {
				guardAllowedNamespaces[n] = true
			}
		}
	}
	log.Printf("NVIDIA_VISIBLE_DEVICES guard: mode=%s allowedNamespaces=%v", guardMode, allowedNamespaceList())
}

func allowedNamespaceList() []string {
	out := make([]string, 0, len(guardAllowedNamespaces))
	for k := range guardAllowedNamespaces {
		out = append(out, k)
	}
	return out
}

// isGpuRuntimeClass reports whether the runtimeClass routes through the NVIDIA
// runtime that honors NVIDIA_VISIBLE_DEVICES (the only pods that can bypass).
func isGpuRuntimeClass(rc string) bool {
	switch rc {
	case "nvidia", "nvidia-cdi", "nvidia-legacy":
		return true
	}
	return false
}

// isAuthorizedGpuPod reports whether a GPU-runtime pod is a legitimate GPU user:
// a KAI share pod, KAI-managed, a GPU-operator system component, or in a trusted ns.
func isAuthorizedGpuPod(pod *corev1.Pod) bool {
	if a := pod.Annotations; a != nil && (a[gpuFractionKey] != "" || a[gpuMemoryKey] != "") {
		return true // KAI fractional-sharing pod
	}
	if l := pod.Labels; l != nil {
		if _, ok := l["kai.scheduler/queue"]; ok {
			return true // KAI-managed
		}
		if l["app.kubernetes.io/managed-by"] == "gpu-operator" {
			return true // GPU operator system component (device-plugin, dcgm, GFD, ...)
		}
	}
	return guardAllowedNamespaces[pod.Namespace]
}

// nvidiaVisibleDevicesGuardOps returns JSON-patch ops that neutralize
// NVIDIA_VISIBLE_DEVICES (=void) on unauthorized GPU-runtime pods, closing the env
// bypass enabled by ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED=true.
// Returns nil when guard is off, the pod uses no GPU runtimeClass, the pod is
// authorized, or in audit mode (logs only).
func nvidiaVisibleDevicesGuardOps(pod *corev1.Pod) []map[string]interface{} {
	if guardMode == "off" {
		return nil
	}
	rc := ""
	if pod.Spec.RuntimeClassName != nil {
		rc = *pod.Spec.RuntimeClassName
	}
	if !isGpuRuntimeClass(rc) {
		return nil // default runtime ignores NVIDIA_VISIBLE_DEVICES — no bypass possible
	}
	if isAuthorizedGpuPod(pod) {
		return nil
	}
	if guardMode == "audit" {
		log.Printf("[nvd-guard][audit] WOULD neutralize NVIDIA_VISIBLE_DEVICES: ns=%s pod=%s runtimeClass=%s", pod.Namespace, podDisplayName(pod), rc)
		return nil
	}
	log.Printf("[nvd-guard][enforce] neutralizing NVIDIA_VISIBLE_DEVICES=void: ns=%s pod=%s runtimeClass=%s", pod.Namespace, podDisplayName(pod), rc)
	var ops []map[string]interface{}
	for i := range pod.Spec.InitContainers {
		ops = append(ops, overrideEnvOps(&pod.Spec.InitContainers[i], "initContainers", i, nvidiaVisibleDevicesEnv, "void")...)
	}
	for i := range pod.Spec.Containers {
		ops = append(ops, overrideEnvOps(&pod.Spec.Containers[i], "containers", i, nvidiaVisibleDevicesEnv, "void")...)
	}
	return ops
}

func podDisplayName(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	return pod.GenerateName + "<generated>"
}

// overrideEnvOps sets env name=value, replacing any existing entry (including a
// valueFrom) so an attacker-set NVIDIA_VISIBLE_DEVICES=all is overridden, and
// handling the empty-env-array case.
func overrideEnvOps(c *corev1.Container, field string, i int, name, value string) []map[string]interface{} {
	envVar := map[string]interface{}{"name": name, "value": value}
	for j := range c.Env {
		if c.Env[j].Name == name {
			return []map[string]interface{}{{
				"op":    "replace",
				"path":  fmt.Sprintf("/spec/%s/%d/env/%d", field, i, j),
				"value": envVar,
			}}
		}
	}
	if len(c.Env) == 0 {
		return []map[string]interface{}{{
			"op":    "add",
			"path":  fmt.Sprintf("/spec/%s/%d/env", field, i),
			"value": []map[string]interface{}{envVar},
		}}
	}
	return []map[string]interface{}{{
		"op":    "add",
		"path":  fmt.Sprintf("/spec/%s/%d/env/-", field, i),
		"value": envVar,
	}}
}
