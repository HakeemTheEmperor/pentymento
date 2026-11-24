# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Toluwalase Akinyemi

GO ?= go

# Tool versions are pinned so every machine and CI run the same checks.
GOLANGCI_LINT_VERSION ?= v2.15.0
STATICCHECK_VERSION ?= v0.8.2
GOVULNCHECK_VERSION ?= v1.10.0
GOSEC_VERSION ?= v2.30.0
GO_LICENSES_VERSION ?= v2.0.1

# Dependency licences compatible with Apache 2.0 (PRD 10.4).
ALLOWED_LICENSES ?= Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

# Build tags that every static check covers, so integration-only code is
# checked too.
TAGS ?= integration

# Commit range checked by `make commits`.
RANGE ?= origin/main..HEAD

# Base revision the message registry is compared with by `make messages`.
BASE ?= origin/main

# The race detector needs cgo and a C compiler. Where cgo is unavailable
# (for example Windows without gcc) tests run without it; CI and WSL run
# with it.
RACE ?= $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)

.PHONY: test test-integration bench lint commits messages licenses vulncheck gosec

## test: unit tests (no Docker needed)
test:
	$(GO) test $(RACE) ./...

## test-integration: unit and database integration tests (needs Docker;
## on Windows, run inside WSL). Select versions with PENTYM_TEST_POSTGRES
## and PENTYM_TEST_MONGODB.
test-integration:
	$(GO) test $(RACE) -tags integration -timeout 30m ./...

## bench: microbenchmarks
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

## lint: go vet, golangci-lint, staticcheck and the licence-header check
lint:
	$(GO) vet -tags $(TAGS) ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags $(TAGS) ./...
	$(GO) run ./internal/devtools/headercheck

## commits: commit-message and DCO check over RANGE
commits:
	$(GO) run ./internal/devtools/commitcheck $(RANGE)

## messages: message registry check: docs entry for every ID, and no ID
## removed, renamed or reused since BASE
messages:
	$(GO) run ./internal/devtools/msgcheck $(BASE)

## licenses: dependency licence check
licenses:
	GOFLAGS=-tags=$(TAGS) $(GO) run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... --allowed_licenses=$(ALLOWED_LICENSES)

## vulncheck: known vulnerabilities in dependencies and the standard library
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags $(TAGS) ./...

## gosec: security static analysis
gosec:
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -quiet -tags $(TAGS) ./...
