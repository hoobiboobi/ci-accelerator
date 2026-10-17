# One-command targets for every check. The same targets run in GitHub Actions.
# Kept compatible with GNU Make 3.81 (the version shipped with macOS).

HELM   ?= helm
CHARTS := charts/buildkit

# Pinned tool versions (see "Research before code" in the workspace notes).
HELM_UNITTEST_VERSION := 1.2.1

# Helm plugins are installed into the repo, not into the user's Helm.
# HELM_DATA_HOME rather than HELM_PLUGINS: the http installer ignores the
# latter (helm/helm#32646, open as of Helm 4.3.1).
TOOLS          := $(CURDIR)/.tools
HELM_DATA_HOME := $(TOOLS)/helm-data
HELM_PLUGINS   := $(HELM_DATA_HOME)/plugins
export HELM_DATA_HOME HELM_PLUGINS

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: tools
tools: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) ## Install pinned helm plugins into .tools/

$(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION):
	@mkdir -p $(HELM_PLUGINS)
	@$(HELM) plugin uninstall unittest >/dev/null 2>&1 || true
	@echo "==> installing helm-unittest $(HELM_UNITTEST_VERSION) into $(HELM_PLUGINS)"
	@$(HELM) plugin install --verify=false \
		https://github.com/helm-unittest/helm-unittest/releases/download/v$(HELM_UNITTEST_VERSION)/unittest-$(HELM_UNITTEST_VERSION).tgz \
		>/dev/null
	@touch $@

.PHONY: lint
lint: ## helm lint --strict every chart
	@for c in $(CHARTS); do \
		echo "==> lint $$c"; \
		$(HELM) lint --strict $$c || exit 1; \
	done

.PHONY: unittest
unittest: tools ## Run helm-unittest suites
	@for c in $(CHARTS); do \
		echo "==> unittest $$c"; \
		$(HELM) unittest --strict $$c || exit 1; \
	done

KIND_CLUSTER ?= buildkit
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)
export KUBE_CONTEXT

.PHONY: kind-up
kind-up: ## Create the kind cluster (if missing) and taint its build nodes
	@kind get clusters 2>/dev/null | grep -qx '$(KIND_CLUSTER)' || \
		kind create cluster --name $(KIND_CLUSTER) --config examples/kind/kind-config.yaml
	@kubectl --context $(KUBE_CONTEXT) taint nodes -l ci-accelerator.io/node-role=build \
		ci-accelerator.io/node-role=build:NoSchedule --overwrite >/dev/null
	@kubectl --context $(KUBE_CONTEXT) get nodes -L ci-accelerator.io/node-role,topology.kubernetes.io/zone

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: e2e
e2e: ## Run end-to-end checks on the kind cluster (KEEP=1 keeps resources)
	@test/e2e/run.sh

.PHONY: template
template: ## Render every chart with default values to stdout
	@for c in $(CHARTS); do \
		echo "# ==> $$c"; \
		$(HELM) template test $$c || exit 1; \
	done
