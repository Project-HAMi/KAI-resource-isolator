#!/usr/bin/env bash
# Copyright The HAMi Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Exercise existing-cluster safety without Docker, kind, or Kubernetes.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
mkdir "$WORKDIR/bin"

cat > "$WORKDIR/bin/fake-cli" <<'SH'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$FAKE_LOG"
case "${0##*/} $*" in
  'kind get clusters') echo kri-smoke ;;
  'kind get nodes --name kri-smoke') echo kri-smoke-control-plane ;;
  'helm version --short') echo "${FAKE_HELM_VERSION:-v4.2.3}" ;;
  helm*' list '*)
    if [ "$FAKE_EXISTING_RELEASE" = 1 ]; then echo kai-resource-isolator; fi
    ;;
  kubectl*' --dry-run=server '*) echo kai-resource-isolator-vgpu ;;
  kubectl*' get ds '*) echo 1 ;;
  kubectl*' wait '*) exit 1 ;;
esac
SH
chmod +x "$WORKDIR/bin/fake-cli"
for cmd in docker kind kubectl helm; do
  ln -s fake-cli "$WORKDIR/bin/$cmd"
done
export FAKE_LOG="$WORKDIR/calls"
export PATH="$WORKDIR/bin:$PATH"

# An existing release must stop the script before build/load, upgrade, or
# namespace deletion. The diagnostic reads on failure are harmless.
: > "$FAKE_LOG"
if FAKE_EXISTING_RELEASE=1 IMAGE=prebuilt KIND_CLUSTER=kri-smoke \
  bash "$REPO_ROOT/hack/e2e/kind-smoke.sh" > "$WORKDIR/output" 2>&1; then
  echo 'expected an existing release to stop the smoke test' >&2
  exit 1
fi
grep -q 'already exists' "$WORKDIR/output" || { cat "$WORKDIR/output" >&2; cat "$FAKE_LOG" >&2; exit 1; }
grep -q 'helm --kube-context kind-kri-smoke list --namespace kai-resource-isolator --short' "$FAKE_LOG"
if grep -Eq '^(docker |helm .* upgrade|kubectl .* delete namespace)' "$FAKE_LOG"; then
  echo 'existing release was modified' >&2
  exit 1
fi

# Helm 3 needs --all to include non-deployed releases; Helm 4 includes all
# statuses by default and no longer accepts that flag.
: > "$FAKE_LOG"
if FAKE_HELM_VERSION=v3.19.0 FAKE_EXISTING_RELEASE=1 IMAGE=prebuilt KIND_CLUSTER=kri-smoke \
  bash "$REPO_ROOT/hack/e2e/kind-smoke.sh" > "$WORKDIR/output" 2>&1; then
  echo 'expected an existing Helm 3 release to stop the smoke test' >&2
  exit 1
fi
grep -q 'already exists' "$WORKDIR/output"
grep -q 'helm --kube-context kind-kri-smoke list --namespace kai-resource-isolator --all --short' "$FAKE_LOG"

# With no existing release the fake run gets past namespace creation, then
# fails its Pod assertions. Cleanup must delete only its own unique namespace.
: > "$FAKE_LOG"
if FAKE_EXISTING_RELEASE=0 IMAGE=prebuilt KIND_CLUSTER=kri-smoke \
  bash "$REPO_ROOT/hack/e2e/kind-smoke.sh" > "$WORKDIR/output" 2>&1; then
  echo 'expected the fake Pod assertions to fail' >&2
  exit 1
fi
created="$(sed -n 's/^kubectl .* create namespace \([^ ]*\)$/\1/p' "$FAKE_LOG")"
deleted="$(sed -n 's/^kubectl .* delete namespace \([^ ]*\) --wait=false$/\1/p' "$FAKE_LOG")"
if [ -z "$created" ] || [ "$created" != "$deleted" ] || [ "$created" = kri-smoke-smoke ]; then
  echo "temporary namespace ownership failed: created='$created' deleted='$deleted'" >&2
  exit 1
fi
printf 'PASS: existing release guarded; only the created namespace was deleted (%s)\n' "$created"
