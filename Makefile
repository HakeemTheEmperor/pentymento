# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Toluwalase Akinyemi

GO ?= go

# Tool versions are pinned so every machine and CI run the same checks.
GOLANGCI_LINT_VERSION ?= v2.15.0
STATICCHECK_VERSION ?= v0.8.2
GOVULNCHECK_VERSION ?= v1.10.0
GOSEC_VERSION ?= v2.30.0
GO_LICENSES_VERSION ?= v2.0.1
BENCHSTAT_VERSION ?= v0.0.0-20261117185222-4febdb082b2c

# Dependency licences compatible with Apache 2.0 (PRD 10.4).
ALLOWED_LICENSES ?= Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

# Dependencies reviewed and allowed by name although their licence is not
# in ALLOWED_LICENSES. Each needs the maintainer's approval:
#   github.com/zeebo/blake3  CC0-1.0 (public-domain dedication, no patent
#                            grant); named by PRD 8.2 as the BLAKE3 library.
LICENSE_EXCEPTIONS ?= github.com/zeebo/blake3

# Build tags that every static check covers, so integration-only code is
# checked too.
TAGS ?= integration

# Extra flags for `make bench`, such as -count=6 -benchtime=200ms.
BENCHFLAGS ?=

# Commit range checked by `make commits`.
RANGE ?= origin/main..HEAD

# Base revision the message registry is compared with by `make messages`.
BASE ?= origin/main

# The race detector needs cgo and a C compiler. Where cgo is unavailable
# (for example Windows without gcc) tests run without it; CI and WSL run
# with it.
RACE ?= $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)

.PHONY: test test-integration bench benchstat lint leaktest commits messages licenses vulncheck gosec

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
	$(GO) test -run '^$$' -bench . -benchmem $(BENCHFLAGS) ./...

## benchstat: compare benchmark results OLD and NEW; time regressions over
## 10% are flagged as warnings
benchstat:
	$(GO) run golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION) $(OLD) $(NEW)
	$(GO) run golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION) -format csv $(OLD) $(NEW) 2>/dev/null | $(GO) run ./internal/devtools/benchcheck

## lint: go vet, golangci-lint, staticcheck and the licence-header check
lint:
	$(GO) vet -tags $(TAGS) ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -tags $(TAGS) ./...
	$(GO) run ./internal/devtools/headercheck

## leaktest: secret-leak tests: run pentym with known fake secrets and scan
## every output for them (tests named TestSecretLeak...)
leaktest:
	$(GO) test $(RACE) -count=1 -run 'SecretLeak' ./...

## commits: commit-message and DCO check over RANGE
commits:
	$(GO) run ./internal/devtools/commitcheck $(RANGE)

## messages: message registry check: docs entry for every ID, and no ID
## removed, renamed or reused since BASE
messages:
	$(GO) run ./internal/devtools/msgcheck $(BASE)

## licenses: dependency licence check
licenses:
	GOFLAGS=-tags=$(TAGS) $(GO) run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... --allowed_licenses=$(ALLOWED_LICENSES) $(addprefix --ignore=,$(LICENSE_EXCEPTIONS))

## vulncheck: known vulnerabilities in dependencies and the standard library
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags $(TAGS) ./...

## gosec: security static analysis
gosec:
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -quiet -tags $(TAGS) ./...
