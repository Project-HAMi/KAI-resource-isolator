#!/usr/bin/env bash
# Copyright The HAMi Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Chart render invariants for the webhook PodDisruptionBudget.
#
# What this protects against: the chart renders a PodDisruptionBudget when
# webhook.replicaCount > 1. When the availability value is read straight from
# .Values.webhook.podDisruptionBudget, a null, empty, negative or out of range
# value either fails the render or produces a spec the API server rejects. The
# worst case renders a PodDisruptionBudget with an EMPTY availability field:
# the API server silently ACCEPTS that object, so a budget that constrains
# nothing is stored as if it were a working budget. Neither failure mode is
# visible to `helm lint` or to `kubectl apply --server-side --dry-run=server`,
# because both accept an empty availability field, which is why this render
# level test is needed.
#
# These cases are expected to FAIL until the fix for the null and out of range
# podDisruptionBudget values lands. Run them against a fixed chart with
# CHART_DIR=/path/to/chart/kai-resource-isolator hack/chart-tests.sh.

set -eu

CHART_DIR="${CHART_DIR:-chart/kai-resource-isolator}"
RELEASE="kai-resource-isolator"

pass_count=0
fail_count=0
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
rendered="$workdir/rendered.yaml"

pass() {
	pass_count=$((pass_count + 1))
	printf 'PASS: %s\n' "$1"
}

fail() {
	fail_count=$((fail_count + 1))
	printf 'FAIL: %s\n' "$1"
	if [ "$#" -gt 1 ]; then
		shift
		printf '      %s\n' "$@"
	fi
}

# render <extra helm template args...>; leaves the output in $rendered and the
# exit status of helm in $render_status.
render() {
	set +e
	helm template "$RELEASE" "$CHART_DIR" "$@" >"$rendered" 2>"$workdir/stderr"
	render_status=$?
	set -e
}

# pdb_count prints how many PodDisruptionBudget documents were rendered.
pdb_count() {
	awk '
		/^---[[:space:]]*$/ { in_pdb = 0; next }
		/^kind:[[:space:]]*PodDisruptionBudget[[:space:]]*$/ { in_pdb = 1; found++ }
		END { print found + 0 }
	' "$rendered"
}

# availability prints "minAvailable <value>" or "maxUnavailable <value>" for
# every availability field of the rendered PodDisruptionBudget, one per line.
# The value is normalised first, so a value that is valid but not lexically
# bare is judged on the scalar the API server sees: a trailing YAML comment is
# cut, and surrounding quotes are stripped only from a percentage. A quoted
# integer is kept quoted, because the API server stores it as a string and
# rejects any string availability value that is not a percentage.
availability() {
	awk '
		BEGIN { sq = sprintf("%c", 39) }
		/^---[[:space:]]*$/ { in_pdb = 0; next }
		/^kind:[[:space:]]*PodDisruptionBudget[[:space:]]*$/ { in_pdb = 1; next }
		in_pdb && /^[[:space:]]+(minAvailable|maxUnavailable):/ {
			line = $0
			sub(/^[[:space:]]+/, "", line)
			sub(/:[[:space:]]*/, ": ", line)
			sub(/[[:space:]]+#.*$/, "", line)
			key = line
			sub(/:.*$/, "", key)
			value = line
			sub(/^[^:]*:[[:space:]]*/, "", value)
			first = substr(value, 1, 1)
			last = substr(value, length(value), 1)
			if (length(value) >= 2 && (first == "\"" || first == sq) && first == last) {
				inner = substr(value, 2, length(value) - 2)
				if (inner ~ /%$/) {
					value = inner
				}
			}
			print key ": " value
		}
	' "$rendered"
}

# valid_availability accepts a non-negative int32 or a percentage in [0, 100].
valid_availability() {
	[ -n "$1" ] || return 1
	case "$1" in
	*%)
		number="${1%%%}"
		case "$number" in
		'' | *[!0-9]*) return 1 ;;
		esac
		[ "$number" -le 100 ] || return 1
		;;
	*)
		case "$1" in
		'' | *[!0-9]*) return 1 ;;
		esac
		# Kubernetes stores the integer arm of IntOrString in an int32.
		digits="$1"
		while [ "${digits#0}" != "$digits" ]; do digits="${digits#0}"; done
		digits="${digits:-0}"
		[ "${#digits}" -le 10 ] && [ "$digits" -le 2147483647 ] || return 1
		;;
	esac
	return 0
}

# render_error prints a one line reason why the last render or check failed.
render_error() {
	if [ "$render_status" -ne 0 ]; then
		sed -e 's/^/      /' "$workdir/stderr" | head -n 5
	fi
}

# check_no_pdb asserts the chart defaults render no PodDisruptionBudget.
check_no_pdb() {
	name="defaults render no PodDisruptionBudget (replicaCount 1)"
	render
	if [ "$render_status" -ne 0 ]; then
		fail "$name" "helm template exited $render_status"
		render_error
		return
	fi
	count="$(pdb_count)"
	if [ "$count" -eq 0 ]; then
		pass "$name"
	else
		fail "$name" "rendered $count PodDisruptionBudget documents, expected 0"
	fi
}

# check_single_pdb_default asserts replicaCount=2 renders exactly one
# PodDisruptionBudget with minAvailable: 1.
check_single_pdb_default() {
	name="replicaCount=2 renders one PodDisruptionBudget with minAvailable 1"
	render --set webhook.replicaCount=2
	if [ "$render_status" -ne 0 ]; then
		fail "$name" "helm template exited $render_status"
		render_error
		return
	fi
	count="$(pdb_count)"
	if [ "$count" -ne 1 ]; then
		fail "$name" "rendered $count PodDisruptionBudget documents, expected 1"
		return
	fi
	fields="$(availability)"
	if [ "$fields" = "minAvailable: 1" ]; then
		pass "$name"
	else
		fail "$name" "availability field is '${fields:-<none>}', expected 'minAvailable: 1'"
	fi
}

# check_invariants <case name> <extra helm template args...> asserts that the
# chart renders and that the PodDisruptionBudget carries exactly one usable
# availability field.
check_invariants() {
	name="$1"
	shift
	render --set webhook.replicaCount=2 "$@"
	if [ "$render_status" -ne 0 ]; then
		fail "$name" "helm template exited $render_status"
		render_error
		return
	fi
	count="$(pdb_count)"
	if [ "$count" -ne 1 ]; then
		fail "$name" "rendered $count PodDisruptionBudget documents, expected 1"
		return
	fi
	fields="$(availability)"
	field_count="$(printf '%s\n' "$fields" | grep -c . || true)"
	if [ "$field_count" -ne 1 ]; then
		fail "$name" "found $field_count availability fields, expected exactly 1: ${fields:-<none>}"
		return
	fi
	key="${fields%%:*}"
	value="${fields#*: }"
	if ! valid_availability "$value"; then
		fail "$name" "$key is '${value}', expected a non-negative int32 or a percentage in [0, 100]"
		return
	fi
	pass "$name"
}

# check_fallback asserts an invalid value renders the safe chart default.
check_fallback() {
	name="$1"
	shift
	render --set webhook.replicaCount=2 "$@"
	if [ "$render_status" -ne 0 ]; then
		fail "$name" "helm template exited $render_status"
		render_error
		return
	fi
	fields="$(availability)"
	if [ "$(pdb_count)" -eq 1 ] && [ "$fields" = "minAvailable: 1" ]; then
		pass "$name"
	else
		fail "$name" "availability field is '${fields:-<none>}', expected 'minAvailable: 1'"
	fi
}

# check_renders_only asserts the chart still renders for the value sets CI
# already covers.
check_renders_only() {
	name="$1"
	shift
	render "$@"
	if [ "$render_status" -ne 0 ]; then
		fail "$name" "helm template exited $render_status"
		render_error
		return
	fi
	pass "$name"
}

if ! command -v helm >/dev/null 2>&1; then
	echo "helm not found on PATH" >&2
	exit 1
fi
if [ ! -f "$CHART_DIR/Chart.yaml" ]; then
	echo "no chart at $CHART_DIR (set CHART_DIR to the chart directory)" >&2
	exit 1
fi

printf 'chart-render invariants: %s\n\n' "$CHART_DIR"

check_no_pdb
check_single_pdb_default

check_invariants 'replicaCount=2 alone' --set webhook.replicaCount=2
check_invariants 'podDisruptionBudget.maxUnavailable=0' --set-string webhook.podDisruptionBudget.maxUnavailable=0
check_invariants 'podDisruptionBudget.maxUnavailable=1' --set-string webhook.podDisruptionBudget.maxUnavailable=1
check_invariants 'podDisruptionBudget.maxUnavailable=50%' --set-string webhook.podDisruptionBudget.maxUnavailable=50%
check_invariants 'podDisruptionBudget.maxUnavailable=100%' --set-string webhook.podDisruptionBudget.maxUnavailable=100%
check_invariants 'podDisruptionBudget.maxUnavailable=2147483647' --set-string webhook.podDisruptionBudget.maxUnavailable=2147483647
check_fallback 'podDisruptionBudget.maxUnavailable=2147483648 falls back' --set-string webhook.podDisruptionBudget.maxUnavailable=2147483648
check_fallback 'podDisruptionBudget.minAvailable=2147483648 falls back' --set-string webhook.podDisruptionBudget.minAvailable=2147483648
check_fallback 'podDisruptionBudget.maxUnavailable=999999999999999999999 falls back' --set-string webhook.podDisruptionBudget.maxUnavailable=999999999999999999999
check_invariants 'podDisruptionBudget.minAvailable=0' --set-string webhook.podDisruptionBudget.minAvailable=0
check_invariants 'podDisruptionBudget.minAvailable=2' --set-string webhook.podDisruptionBudget.minAvailable=2
check_invariants 'podDisruptionBudget={}' --set-json 'webhook.podDisruptionBudget={}'
check_invariants 'podDisruptionBudget=null' --set-json 'webhook.podDisruptionBudget=null'
check_invariants 'podDisruptionBudget.minAvailable=null' --set-json 'webhook.podDisruptionBudget.minAvailable=null'
check_invariants 'podDisruptionBudget.maxUnavailable= (empty)' --set-string 'webhook.podDisruptionBudget.maxUnavailable='
check_invariants 'podDisruptionBudget.maxUnavailable="" (quoted empty)' --set-string 'webhook.podDisruptionBudget.maxUnavailable=""'
check_invariants 'podDisruptionBudget.minAvailable="" (quoted empty)' --set-string 'webhook.podDisruptionBudget.minAvailable=""'
check_invariants 'podDisruptionBudget.maxUnavailable=abc' --set-string webhook.podDisruptionBudget.maxUnavailable=abc
check_invariants 'podDisruptionBudget.maxUnavailable=-1' --set-string webhook.podDisruptionBudget.maxUnavailable=-1
check_invariants 'podDisruptionBudget.maxUnavailable=150%' --set-string webhook.podDisruptionBudget.maxUnavailable=150%

check_renders_only 'CI value set: chart defaults render'
check_renders_only 'CI value set: optional values render' --set monitor.enabled=true,monitor.serviceMonitor.enabled=true,tls.certManager.enabled=true,tls.patch.enabled=false

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[ "$fail_count" -eq 0 ]
