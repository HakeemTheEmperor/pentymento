# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Toluwalase Akinyemi

GO ?= go

# Linter versions are pinned so every machine and CI run the same checks.
GOLANGCI_LINT_VERSION ?= v2.15.0
STATICCHECK_VERSION ?= v0.8.2

# The race detector needs cgo and a C compiler. Where cgo is unavailable
# (for example Windows without gcc) tests run without it; CI and WSL run
# with it.
RACE ?= $(if $(filter 1,$(shell $(GO) env CGO_ENABLED)),-race,)

.PHONY: test test-integration bench lint

## test: unit tests (no Docker needed)
test:
	$(GO) test $(RACE) ./...

## test-integration: unit and database integration tests (needs Docker;
## on Windows, run inside WSL)
test-integration:
	$(GO) test $(RACE) -tags integration ./...

## bench: microbenchmarks
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

## lint: go vet, golangci-lint and staticcheck
lint:
	$(GO) vet ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
