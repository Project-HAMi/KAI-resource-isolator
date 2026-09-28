#!/usr/bin/env bash
# Copyright The HAMi Authors.
# SPDX-License-Identifier: Apache-2.0
#
# GPU-less kind smoke test for the kai-resource-isolator chart.
#
# Why this exists: the KAI-Scheduler e2e cannot cover any of this. KAI PR #2033
# installs this chart in its hamicore e2e, but the e2e is "intentionally not
# wired into CI for now" because it runs against a fake GPU operator, and KAI
# maintainer davidLif declined to test HAMi metrics there. Nothing else exercises
# the chart: webhook injection, the libsync file placement on the node and the
# permissions of the non-root cache directory are otherwise untested, which is
# how the non-root mkdir bug behind the stale-closed PR #22 went unnoticed.
# This script therefore tests the chart on a plain kind cluster, with no GPU, no
# KAI install and no cloud credentials, and complements the KAI e2e instead of
# duplicating it.
#
# How to run (from anywhere inside the repository):
#   hack/e2e/kind-smoke.sh
#
# What each check proves:
#   a  libsync copies a non-empty libvgpu.so and a matching ld.so.preload into
#      {hostInstallBase}/vgpu on the node, and the DaemonSet is Ready
#   b  the MutatingWebhookConfiguration named <release>-mutating injects the three
#      volumes, the three mounts, POD_UID/CONTAINER_NAME/CONTAINER_VGPU_MOUNT and
#      an /etc/ld.so.preload referencing libvgpu.so into a gpu-fraction pod
#   c  the preloaded pod runs as uid 1000, actually loads libvgpu.so and can
#      create its own cache directory {CONTAINER_VGPU_MOUNT}/containers/
#      {POD_UID}_{CONTAINER_NAME} (libvgpu falls back to /tmp/cudevshr.cache and
#      logs errno=13 when this fails)
#   d  pods without annotations and pods with kai-resource-isolator.io/inject
#      "false" are left untouched
#
# Note: on chart versions from before the libsync cache-directory fix, check c4
# fails by design (that is the bug this script exists to catch); run with
# EXPECT_NONROOT_WRITE=0 to report it as XFAIL instead.
#
# The kind cluster is deleted when the script exits, unless KEEP_CLUSTER=1. A
# cluster that already existed when the script started is never deleted.
#
# Environment knobs:
#   KIND_CLUSTER          kind cluster name (default: kri-smoke)
#   KEEP_CLUSTER          1 leaves the cluster behind for inspection (default: 0)
#   IMAGE                 prebuilt image to load into kind; skips the docker build
#                         (default: build kai-resource-isolator:e2e from
#                         hack/e2e/Dockerfile)
#   CHART_DIR             chart path (default: <repo>/chart/kai-resource-isolator)
#   RELEASE               helm release name (default: kai-resource-isolator)
#   NAMESPACE             release namespace (default: kai-resource-isolator)
#   HELM_EXTRA_ARGS       extra args appended to helm upgrade --install (word-split)
#   TIMEOUT               kubectl/helm timeout (default: 180s)
#   EXPECT_NONROOT_WRITE  1 (default) fails the run when check c4 cannot create the
#                         cache dir; 0 reports it as XFAIL instead, which is what a
#                         baseline run against a chart without the fix wants

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-kri-smoke}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
IMAGE="${IMAGE:-}"
CHART_DIR="${CHART_DIR:-${REPO_ROOT}/chart/kai-resource-isolator}"
RELEASE="${RELEASE:-kai-resource-isolator}"
NAMESPACE="${NAMESPACE:-kai-resource-isolator}"
HELM_EXTRA_ARGS="${HELM_EXTRA_ARGS:-}"
TIMEOUT="${TIMEOUT:-180s}"
EXPECT_NONROOT_WRITE="${EXPECT_NONROOT_WRITE:-1}"

CTX="kind-${KIND_CLUSTER}"
TEST_NS="${KIND_CLUSTER}-smoke"
TEST_POD_IMAGE="ubuntu:24.04"
VGPU_MOUNT="/usr/local/vgpu"
CREATED_CLUSTER=0
FAILED=0
START_TS="$(date +%s)"
RESULTS=()

log() { echo "[kind-smoke] $*"; }
kc() { kubectl --context "${CTX}" "$@"; }

record() {
	# record <PASS|FAIL|XFAIL> <name> [detail]
	local status="$1" name="$2" detail="${3:-}"
	RESULTS+=("${status} ${name}${detail:+ (${detail})}")
	[[ "${status}" == "FAIL" ]] && FAILED=1
	echo "  ${status}: ${name}${detail:+ (${detail})}"
	return 0
}

check() {
	# check <name> <command...>
	local name="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		record PASS "${name}"
	else
		record FAIL "${name}"
	fi
}

one_line() { tr '\n' ' ' <<<"$1" | sed 's/  */ /g'; }

cleanup() {
	local rc=$?
	if [[ "${rc}" -ne 0 ]] && kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
		log "diagnostics: webhook logs"
		kc -n "${NAMESPACE}" logs "deploy/${RELEASE}-webhook" --tail=50 2>/dev/null || true
		kc get pods -A 2>/dev/null || true
	fi
	if [[ "${KEEP_CLUSTER}" == "1" ]]; then
		log "KEEP_CLUSTER=1: leaving cluster ${KIND_CLUSTER} (context ${CTX})"
	elif [[ "${CREATED_CLUSTER}" == "1" ]]; then
		log "deleting kind cluster ${KIND_CLUSTER}"
		kind delete cluster --name "${KIND_CLUSTER}" >/dev/null 2>&1 || true
	fi
	log "runtime: $(($(date +%s) - START_TS))s"
	exit "${rc}"
}
trap cleanup EXIT

for bin in docker kind kubectl helm; do
	command -v "${bin}" >/dev/null || {
		echo "missing required tool: ${bin}" >&2
		exit 1
	}
done

# --- cluster ---------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
	log "reusing existing kind cluster ${KIND_CLUSTER}"
else
	log "creating kind cluster ${KIND_CLUSTER}"
	kind create cluster --name "${KIND_CLUSTER}" --wait 120s
	CREATED_CLUSTER=1
fi
NODE="$(kind get nodes --name "${KIND_CLUSTER}" | head -n1)"

# --- image -----------------------------------------------------------------
if [[ -z "${IMAGE}" ]]; then
	IMAGE="kai-resource-isolator:e2e"
	log "building ${IMAGE} from hack/e2e/Dockerfile"
	docker build -f "${REPO_ROOT}/hack/e2e/Dockerfile" -t "${IMAGE}" "${REPO_ROOT}"
fi
log "loading ${IMAGE} into kind"
kind load docker-image --name "${KIND_CLUSTER}" "${IMAGE}"

# Split IMAGE into registry / repository / tag for the chart values.
img_registry=""
img_tag="latest"
if [[ "${IMAGE}" == *:* && "${IMAGE##*/}" == *:* ]]; then
	img_repo="${IMAGE%:*}"
	img_tag="${IMAGE##*:}"
else
	img_repo="${IMAGE}"
fi
if [[ "${img_repo}" == */* ]]; then
	first="${img_repo%%/*}"
	if [[ "${first}" == *.* || "${first}" == *:* || "${first}" == localhost ]]; then
		img_registry="${first}"
		img_repo="${img_repo#*/}"
	fi
fi

# --- install ---------------------------------------------------------------
# helm returns only after its hook Jobs (certs, caBundle patch) have completed,
# so the Deployment and DaemonSet rollouts below see a usable MutatingWebhookConfiguration.
log "helm install ${CHART_DIR} release=${RELEASE} ns=${NAMESPACE} image=${IMAGE} extra='${HELM_EXTRA_ARGS}'"
# shellcheck disable=SC2086 # HELM_EXTRA_ARGS is intentionally word-split
helm --kube-context "${CTX}" upgrade --install "${RELEASE}" "${CHART_DIR}" \
	--namespace "${NAMESPACE}" --create-namespace \
	--set-string "image.registry=${img_registry}" \
	--set-string "image.repository=${img_repo}" \
	--set-string "image.tag=${img_tag}" \
	--set image.pullPolicy=IfNotPresent \
	--timeout "${TIMEOUT}" ${HELM_EXTRA_ARGS}

kc -n "${NAMESPACE}" rollout status "deploy/${RELEASE}-webhook" --timeout "${TIMEOUT}"
kc -n "${NAMESPACE}" rollout status "ds/${RELEASE}-libsync" --timeout "${TIMEOUT}"

kc delete namespace "${TEST_NS}" --ignore-not-found --wait=true --timeout "${TIMEOUT}" >/dev/null
kc create namespace "${TEST_NS}" >/dev/null

pod_manifest() {
	# pod_manifest <name> <annotations block, already indented>
	cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${TEST_NS}
  annotations:
    kind-smoke: "true"
$2
spec:
  terminationGracePeriodSeconds: 0
  securityContext:
    runAsUser: 1000
  containers:
    - name: main
      image: ${TEST_POD_IMAGE}
      command: ["sleep", "infinity"]
EOF
}

jp() { kc -n "${TEST_NS}" get pod "$1" -o jsonpath="$2"; }

# failurePolicy is Ignore, so a pod can be admitted before the webhook serves.
# Wait until a server-side dry run actually comes back mutated.
log "waiting for ${RELEASE}-mutating to serve mutations"
ready=0
for _ in $(seq 1 60); do
	if pod_manifest probe '    gpu-fraction: "0.5"' |
		kc create --dry-run=server -f - -o jsonpath='{.spec.volumes[*].name}' 2>/dev/null |
		grep -q kai-resource-isolator-vgpu; then
		ready=1
		break
	fi
	sleep 2
done
[[ "${ready}" == "1" ]] || log "WARNING: webhook did not mutate a dry-run pod within 120s"

echo
log "=== assertions ==="

# a: libsync places the library on the node ---------------------------------
echo "[a] libsync DaemonSet and node files"
desired="$(kc -n "${NAMESPACE}" get ds "${RELEASE}-libsync" -o jsonpath='{.status.desiredNumberScheduled}')"
number_ready="$(kc -n "${NAMESPACE}" get ds "${RELEASE}-libsync" -o jsonpath='{.status.numberReady}')"
if [[ -n "${desired}" && "${desired}" != "0" && "${desired}" == "${number_ready}" ]]; then
	record PASS "a1 libsync DaemonSet Ready" "${number_ready}/${desired}"
else
	record FAIL "a1 libsync DaemonSet Ready" "${number_ready:-0}/${desired:-0}"
fi
check "a2 ${NODE}:${VGPU_MOUNT}/libvgpu.so is non-empty" docker exec "${NODE}" test -s "${VGPU_MOUNT}/libvgpu.so"
check "a3 ${NODE}:${VGPU_MOUNT}/ld.so.preload references libvgpu.so" docker exec "${NODE}" grep -q libvgpu.so "${VGPU_MOUNT}/ld.so.preload"

# b: webhook injection -------------------------------------------------------
echo "[b] gpu-fraction pod is mutated"
pod_manifest frac '    gpu-fraction: "0.5"' | kc apply -f - >/dev/null
vols="$(jp frac '{.spec.volumes[*].name}')"
for v in kai-resource-isolator-vgpu kai-resource-isolator-containers kai-resource-isolator-vgpulock; do
	if [[ " ${vols} " == *" ${v} "* ]]; then
		record PASS "b1 injected volume ${v}"
	else
		record FAIL "b1 injected volume ${v}" "volumes: ${vols}"
	fi
done
mounts="$(jp frac '{range .spec.containers[0].volumeMounts[*]}{.name}={.mountPath}{"\n"}{end}')"
mounts_line="$(one_line "${mounts}")"
for m in "kai-resource-isolator-vgpu=${VGPU_MOUNT}" \
	"kai-resource-isolator-vgpu=/etc/ld.so.preload" \
	"kai-resource-isolator-containers=${VGPU_MOUNT}/containers" \
	"kai-resource-isolator-vgpulock=/tmp/vgpulock"; do
	if grep -qx "${m}" <<<"${mounts}"; then
		record PASS "b2 injected mount ${m}"
	else
		record FAIL "b2 injected mount ${m}" "mounts: ${mounts_line}"
	fi
done
envs="$(jp frac '{.spec.containers[0].env[*].name}')"
for e in POD_UID CONTAINER_NAME CONTAINER_VGPU_MOUNT; do
	if [[ " ${envs} " == *" ${e} "* ]]; then
		record PASS "b3 injected env ${e}"
	else
		record FAIL "b3 injected env ${e}" "env: ${envs}"
	fi
done

# c: non-root cache dir ------------------------------------------------------
echo "[c] uid 1000 inside the preloaded pod"
if kc -n "${TEST_NS}" wait --for=condition=Ready "pod/frac" --timeout "${TIMEOUT}" >/dev/null 2>&1; then
	restarts="$(jp frac '{.status.containerStatuses[0].restartCount}')"
	if [[ "${restarts}" == "0" ]]; then
		record PASS "c1 preloaded pod runs without a GPU" "restarts=0"
	else
		record FAIL "c1 preloaded pod runs without a GPU" "restarts=${restarts}"
	fi
	preload="$(kc -n "${TEST_NS}" exec frac -- cat /etc/ld.so.preload 2>&1 || true)"
	preload_line="$(one_line "${preload}")"
	if grep -q libvgpu.so <<<"${preload}"; then
		record PASS "c2 pod /etc/ld.so.preload references libvgpu.so"
	else
		record FAIL "c2 pod /etc/ld.so.preload references libvgpu.so" "${preload_line}"
	fi
	maps="$(kc -n "${TEST_NS}" exec frac -- grep -c libvgpu.so /proc/1/maps 2>/dev/null || true)"
	if [[ "${maps:-0}" -gt 0 ]]; then
		record PASS "c3 libvgpu.so is mapped into the preloaded process"
	else
		record FAIL "c3 libvgpu.so is mapped into the preloaded process"
	fi
	# shellcheck disable=SC2016 # single quotes: expanded by the shell inside the pod
	if out="$(kc -n "${TEST_NS}" exec frac -- sh -c \
		'mkdir -p "$CONTAINER_VGPU_MOUNT/containers/${POD_UID}_${CONTAINER_NAME}"' 2>&1)"; then
		record PASS "c4 uid 1000 mkdir containers/\${POD_UID}_\${CONTAINER_NAME}"
	else
		perms="$(docker exec "${NODE}" stat -c '%a %U:%G' "${VGPU_MOUNT}/containers" 2>/dev/null || echo absent)"
		detail="$(one_line "${out}"), host ${VGPU_MOUNT}/containers is ${perms}"
		if [[ "${EXPECT_NONROOT_WRITE}" == "1" ]]; then
			record FAIL "c4 uid 1000 mkdir containers/\${POD_UID}_\${CONTAINER_NAME}" "${detail}"
		else
			record XFAIL "c4 uid 1000 mkdir containers/\${POD_UID}_\${CONTAINER_NAME}" "${detail}"
		fi
	fi
else
	record FAIL "c1 preloaded pod runs without a GPU" "pod/frac not Ready within ${TIMEOUT}"
	kc -n "${TEST_NS}" describe pod frac 2>/dev/null | tail -n 20 || true
	record FAIL "c4 uid 1000 mkdir containers/\${POD_UID}_\${CONTAINER_NAME}" "pod/frac not Ready"
fi

# d: opt-out paths -----------------------------------------------------------
echo "[d] pods that must not be mutated"
pod_manifest plain '' | kc apply -f - >/dev/null
pod_manifest optout '    gpu-fraction: "0.5"
    kai-resource-isolator.io/inject: "false"' | kc apply -f - >/dev/null
for p in plain optout; do
	spec="$(jp "${p}" '{.spec.volumes[*].name} {.spec.containers[0].volumeMounts[*].mountPath} {.spec.containers[0].env[*].name}')"
	spec_line="$(one_line "${spec}")"
	if [[ "${spec}" == *kai-resource-isolator* || "${spec}" == *ld.so.preload* || "${spec}" == *CONTAINER_VGPU_MOUNT* ]]; then
		record FAIL "d1 ${p} pod not mutated" "${spec_line}"
	else
		record PASS "d1 ${p} pod not mutated"
	fi
done

kc delete namespace "${TEST_NS}" --wait=false >/dev/null 2>&1 || true

# summary --------------------------------------------------------------------
echo
log "=== summary ==="
for r in ${RESULTS[@]+"${RESULTS[@]}"}; do echo "  ${r}"; done
if [[ "${FAILED}" == "1" ]]; then
	log "RESULT: FAIL"
	exit 1
fi
log "RESULT: PASS"
