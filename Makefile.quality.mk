# Quality gates: format, lint, vet, verify, test, precommit, release prep.

.PHONY: setup-envtest
setup-envtest: envtest
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
		}

# Run go fmt against code
fmt:
	@for dir in . qpext kernelcache/mcv; do \
	  echo "Formatting $$dir..."; \
	  (cd $$dir && go fmt ./...) || exit 1; \
	done

py-fmt: $(RUFF)
	$(RUFF) format --config ruff.toml ./python ./docs ./test ./hack

# Run go vet against code
vet:
	go vet ./pkg/... ./cmd/... && cd qpext && go vet ./...
	@if pkg-config --exists gpgme 2>/dev/null; then \
		echo "Vetting kernelcache/mcv..."; \
		(cd kernelcache/mcv && CGO_ENABLED=1 go vet ./...) || exit 1; \
	else \
		echo "Skipping kernelcache/mcv vet (CGO dependencies not installed, covered by mcv-build-test CI)"; \
	fi

tidy:
	@for dir in . qpext kernelcache/mcv; do \
	  echo "Tidying $$dir..."; \
	  (cd $$dir && go mod tidy) || exit 1; \
	done

go-lint: golangci-lint
	@for dir in . qpext; do \
	  echo "Linting $$dir..."; \
	  (cd $$dir && $(GOLANGCI_LINT) run --fix) || exit 1; \
	done
	@if pkg-config --exists gpgme 2>/dev/null; then \
		echo "Linting kernelcache/mcv..."; \
		(cd kernelcache/mcv && CGO_ENABLED=1 $(GOLANGCI_LINT) run --fix) || exit 1; \
	else \
		echo "Skipping kernelcache/mcv lint (CGO dependencies not installed, covered by mcv-build-test CI)"; \
	fi

py-lint: $(RUFF)
	$(RUFF) check --config ruff.toml

# Shell scripts to lint. Expressed as exclusions so a newly added script is
# linted by default instead of having to be opted in:
#   install/**                    - frozen per-release install snapshots, never edited after a release is cut
#   hack/setup/quick-install/*.sh - written by `make generate-quick-install-scripts`
# Using `git ls-files` also means gitignored output (bin/, venvs, ...) is skipped for free.
SHELLCHECK_EXCLUDES := ':(exclude)install/**' ':(exclude)hack/setup/quick-install/*.sh'

# Gate at `error`, the severity the tree is already clean at. Tightening to
# `warning` is a bounded follow-up (mostly SC2155/SC2206/SC2034); run
# `make sh-lint SHELLCHECK_SEVERITY=warning` to see that backlog.
SHELLCHECK_SEVERITY ?= error
# ShellCheck is CPU-bound per script; keep parallelism bounded for laptops and CI.
SHELLCHECK_JOBS ?= 4
SHELLCHECK_BATCH_SIZE ?= 8

.PHONY: sh-lint
sh-lint: $(SHELLCHECK)
	@echo "Shell-linting scripts (severity: $(SHELLCHECK_SEVERITY))"
	@git ls-files -z --cached --others --exclude-standard -- '*.sh' $(SHELLCHECK_EXCLUDES) \
		| xargs -0 -r -n $(SHELLCHECK_BATCH_SIZE) -P $(SHELLCHECK_JOBS) $(SHELLCHECK) --severity=$(SHELLCHECK_SEVERITY)

# Verify e2e test files parse and collect without errors (catches import errors, syntax errors, fixture issues).
e2e-collect: $(PYTEST)
	$(UV) pip install --python $(PYTHON_BIN)/python -e ./python/kserve -q
	$(UV) pip install --python $(PYTHON_BIN)/python --group test --directory ./python/kserve -q
	$(PYTEST) --collect-only test/e2e/ -q

pin-actions: pinact
	GITHUB_TOKEN=$$(gh auth token 2>/dev/null) $(PINACT) run .github/workflows/*.yml .github/workflows/*.yaml

# Verify that all GitHub Actions are pinned to a full-length commit SHA (offline check, no API calls).
verify-pinned-actions:
	@if grep -rPn 'uses:\s+\S+@(?!([0-9a-f]{40}))' .github/workflows/*.yml .github/workflows/*.yaml 2>/dev/null; then \
		echo "ERROR: Found GitHub Actions not pinned to a full SHA. Run 'make pin-actions' to fix."; \
		exit 1; \
	fi

validate-infra-scripts:
	@python3 hack/setup/scripts/validate-install-scripts.py

lint-helm-charts:
	@bash hack/setup/scripts/lint-helm.sh

verify-helm-helpers-consistency:
	@bash hack/setup/scripts/verify-helm-helpers.sh

verify-minimal-crd-sync:
	@bash hack/verify-minimal-crd-sync.sh

.PHONY: ensure-go-version-upgrade ensure-golangci-go-version
ensure-go-version-upgrade: ensure-golangci-go-version

ensure-golangci-go-version: yq	
	@GO_GOMOD_VERSION="$$(grep -m1 '^go ' go.mod | cut -d' ' -f2 | cut -d. -f1-2)"; \
	GO_GOLANGCI_VERSION="$$($(YQ) -r '.run.go // ""' .golangci.yml | cut -d. -f1-2)"; \
	if [ -z "$${GO_GOLANGCI_VERSION}" ]; then \
		echo "INFO: '.golangci.yml:run.go' is not set; defaulting to $$GO_GOMOD_VERSION."; \
		GO_GOLANGCI_VERSION="$${GO_GOMOD_VERSION}"; \
	fi; \
	if [ "$${GO_GOMOD_VERSION}" != "$${GO_GOLANGCI_VERSION}" ]; then \
		echo "ERROR: go.mod uses Go $$GO_GOMOD_VERSION but .golangci.yml uses $$GO_GOLANGCI_VERSION"; \
		echo "Please update '.golangci.yml:run.go' to $$GO_GOMOD_VERSION (major.minor) and rerun 'make precommit'."; \
		exit 1; \
	fi
# Sync common helpers to all charts (must run before helm package)
sync-helm-common-helpers:
	@echo "Syncing common helpers to all charts..."
	@for chart in kserve-resources kserve-llmisvc-resources kserve-localmodel-resources kserve-runtime-configs; do \
		cp charts/_common/_utils.tpl charts/$$chart/templates/_utils.tpl; \
		echo "  ✓ Copied to charts/$$chart/templates/_utils.tpl"; \
	done

# Configuration: in setup.cfg [codespell] section (skip paths, ignore-words-list)
spell-check: $(CODESPELL)
	$(CODESPELL)

# Tools the precommit graph runs, installed up front by precommit-prelude.
PRECOMMIT_TOOLS = $(YQ) $(KUSTOMIZE) $(CONTROLLER_GEN) $(HELM_DOCS) $(GOLANGCI_LINT) $(SHELLCHECK) \
	$(UV) $(RUFF) $(PYTEST) $(CODESPELL)

# Targets precommit runs in parallel, longest chain first. vet is not among them:
# golangci-lint's govet runs the same analyzers, and `make test` still runs vet.
PRECOMMIT_TARGETS = generate manifests go-lint tidy \
	ensure-go-version-upgrade py-fmt py-lint sh-lint e2e-collect uv-lock \
	generate-quick-install-scripts generate-chart-manifests generate-chart-docs \
	sync-helm-common-helpers sync-helm-common-resource-helpers sync-helm-multi-resource-helpers \
	verify-pinned-actions verify-minimal-crd-sync boilerplate spell-check

# PRECOMMIT_JOBS=1 runs the graph serially. Prefer it over `make -j1`, which make 3.81
# does not pass on to the graph.
PRECOMMIT_JOBS ?= $(shell nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 1)

# This runs all necessary steps to prepare for a commit.
#
# kserve-deps.env is included at parse time, so the tool versions and GIE_VERSION
# that sync-deps writes are only seen by a make started after it. The prelude
# installs tools one at a time: sync-deps rewrites kserve-deps.env, which re-runs
# the venv tools' pip install, and concurrent pip installs into one venv are not
# safe. pip leaves those tools older than kserve-deps.env, so the graph treats
# them as up to date (-o) rather than reinstalling them in parallel. A -j or -O
# from the calling make wins over PRECOMMIT_JOBS and --output-sync=target, so the
# graph shares its jobserver.
precommit: sync-deps
	@$(MAKE) --no-print-directory PRECOMMIT_SUBMAKE=1 precommit-prelude
	@$(MAKE) --no-print-directory PRECOMMIT_SUBMAKE=1 $(if $(filter -j%,$(MAKEFLAGS)),,-j$(PRECOMMIT_JOBS)) \
		$(if $(filter -O%,$(MAKEFLAGS)),,$(if $(filter output-sync,$(.FEATURES)),--output-sync=target)) \
		$(addprefix -o ,$(PRECOMMIT_TOOLS)) precommit-graph

.PHONY: precommit-prelude precommit-graph
precommit-prelude: sync-img-env $(PRECOMMIT_TOOLS)
precommit-graph: $(PRECOMMIT_TARGETS)

ifneq (,$(filter precommit-prelude,$(MAKECMDGOALS)))
.NOTPARALLEL:
endif

# Ordering edges for the graph. They are declared only when it is the goal, so a
# target run on its own keeps its usual behaviour.
ifneq (,$(filter precommit-graph,$(MAKECMDGOALS)))
ifeq (,$(PRECOMMIT_SUBMAKE))
$(error precommit-graph relies on the tools precommit installs first; run make precommit)
endif
# Both truncate and rewrite Go under pkg/apis (gengo zz_generated.defaults.go,
# controller-gen zz_generated.deepcopy.go) and fail to load it mid-write.
manifests: | generate
# golangci-lint type-checks the generated Go.
go-lint: | generate manifests
# go commands under GOFLAGS=-mod=mod add go.sum lines (golangci-lint adds about 530
# on a cold cache); tidy has to run after them to drop those lines.
tidy: | generate manifests go-lint
# client-gen rewrites python/kserve, which ruff formats.
py-fmt: | generate
# These read or run Python that ruff format rewrites.
py-lint e2e-collect validate-infra-scripts generate-quick-install-scripts: | py-fmt
# Both pip install into bin/.venv.
generate-quick-install-scripts: | e2e-collect
# Both build config/** with kustomize; chart generation also helm-lints the synced helpers.
generate-quick-install-scripts: | manifests
generate-chart-manifests: | manifests sync-helm-common-helpers sync-helm-common-resource-helpers sync-helm-multi-resource-helpers
# helm-docs reads the values.yaml files chart generation writes.
generate-chart-docs: | generate-chart-manifests
# Compares the CRD directories manifests writes.
verify-minimal-crd-sync: | manifests
# boilerplate adds headers to Go and Python files these targets write.
boilerplate: | generate manifests go-lint py-fmt
# codespell reads nearly every file, uv.lock included, so it runs last.
spell-check: | $(filter-out spell-check,$(PRECOMMIT_TARGETS))
endif

# This is used by CI to ensure that the precommit checks are met.
check: precommit
	@if [ ! -z "`git status -s`" ]; then \
		echo "The following differences will fail CI until committed:"; \
		git diff --exit-code; \
		echo "Please ensure that you have run 'make precommit' and committed the changes."; \
		exit 1; \
	fi

# Run tests
# Override TEST_PKGS to focus on specific packages, e.g.:
#   make test TEST_PKGS="./pkg/controller/v1alpha2/llmisvc/..."
TEST_PKGS ?= $$(go list ./pkg/...) ./cmd/...
TEST_TIMEOUT ?= 30m
test: fmt vet manifests envtest test-qpext
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test --timeout $(TEST_TIMEOUT) $(TEST_PKGS) -coverprofile coverage.out -coverpkg ./pkg/... ./cmd...

test-qpext:
	cd qpext && go test -v ./... -cover

bump-version:
	@echo "bumping version numbers for this release"
	@hack/release/prepare-for-release.sh $(PRIOR_VERSION) $(NEW_VERSION)

.PHONY: check-doc-links
check-doc-links:
	@python3 hack/verify-doc-links.py && echo "$@: OK"

# Replays the CLI flags our manifests render against every image whose tag
# changed versus BASE_REF. Needs a container engine and network access, so it
# is not part of precommit.
.PHONY: check-image-flag-drift
check-image-flag-drift: yq
	@BASE_REF=$(or $(BASE_REF),origin/master) HEAD_REF=$(or $(HEAD_REF),HEAD) \
		ENGINE=$(ENGINE) YQ=$(YQ) \
		hack/verify-image-flag-drift.sh && echo "$@: OK"
