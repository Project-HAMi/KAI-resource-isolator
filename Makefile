# Copyright The HAMi Authors.
# SPDX-License-Identifier: Apache-2.0

CHART := chart/kai-resource-isolator

.PHONY: all
all: verify

## test: run the Go unit tests
.PHONY: test
test:
	go test ./...

## lint: run golangci-lint (needs golangci-lint on PATH, e.g. ~/go/bin/golangci-lint)
.PHONY: lint
lint:
	golangci-lint run

## helm-lint: run helm lint on the chart (needs helm on PATH)
.PHONY: helm-lint
helm-lint:
	helm lint $(CHART)

## helm-template: render the chart with defaults and with the optional values enabled
## (output is discarded, this target only checks that the chart renders)
.PHONY: helm-template
helm-template:
	helm template kai-resource-isolator $(CHART) > /dev/null
	helm template kai-resource-isolator $(CHART) --set monitor.enabled=true,monitor.serviceMonitor.enabled=true,tls.certManager.enabled=true,tls.patch.enabled=false > /dev/null

## verify: run the tests and the chart checks; lint is a separate target because
## it needs golangci-lint on PATH, which CI does not install (as in HAMi, where
## verify and lint are separate)
.PHONY: verify
verify: test helm-lint helm-template
