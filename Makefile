# One-command targets for every check; CI is meant to run the same targets.
# Works with GNU Make 3.81 (macOS) and 4.x (Linux), Helm 3 and Helm 4.

HELM   ?= helm
CHARTS := charts/buildkit
# Extra values files every chart check also renders with.
CHART_VALUES := examples/kind/values.yaml test/e2e/values.yaml
# Kubernetes versions the rendered manifests are validated against (the
# chart's minimum and the newest tested).
KUBE_VERSIONS := 1.34.0 1.37.0

# Pinned tools. Bump the version and its checksums together.
HELM_UNITTEST_VERSION := 1.2.1
# sha256 of unittest-<version>.tgz, as listed in the release's signed .prov.
HELM_UNITTEST_SHA256 := bbcebd42631dff9fc595462fad839c49a1b06f9635e952e7e837d7e5fe82865f
KUBECONFORM_VERSION := 0.8.0
# sha256 per platform, from the release's CHECKSUMS file.
KUBECONFORM_SHA256_darwin_arm64 := f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67
KUBECONFORM_SHA256_darwin_amd64 := 71dbc87ac9f24099a62b93570e65aa06312ba6ac8aea63b7f86e9d999edf5a92
KUBECONFORM_SHA256_linux_arm64  := 1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87
KUBECONFORM_SHA256_linux_amd64  := 9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883
SHELLCHECK_VERSION := 0.11.0
# sha256 per platform; the release publishes none, these were recorded at the
# first download (2026-10-18).
SHELLCHECK_SHA256_darwin_arm64 := 339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f
SHELLCHECK_SHA256_darwin_amd64 := c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51
SHELLCHECK_SHA256_linux_arm64  := 68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc
SHELLCHECK_SHA256_linux_amd64  := b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6

OS   := $(shell uname -s | tr A-Z a-z)
ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
SC_ARCH := $(if $(filter amd64,$(ARCH)),x86_64,aarch64)
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)

# Tools live in the repo, not in the user's Helm or PATH. HELM_DATA_HOME
# rather than HELM_PLUGINS: Helm 4's installers ignore the latter
# (helm/helm#32646).
TOOLS          := $(CURDIR)/.tools
HELM_DATA_HOME := $(TOOLS)/helm-data
HELM_PLUGINS   := $(HELM_DATA_HOME)/plugins
export HELM_DATA_HOME HELM_PLUGINS
KUBECONFORM := $(TOOLS)/kubeconform-$(KUBECONFORM_VERSION)/kubeconform
SHELLCHECK  := $(TOOLS)/shellcheck-v$(SHELLCHECK_VERSION)/shellcheck

# kind cluster for e2e. The context is derived from the cluster name and never
# taken from the environment, so `make` cannot be pointed at another cluster.
KIND_CLUSTER ?= buildkit
KIND_CONFIG  ?= examples/kind/kind-config.yaml
# Node image override, e.g. kindest/node:v1.34.11@sha256:... (minimum version).
KIND_IMAGE   ?=
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
export KUBE_CONTEXT

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

# fetch <url> <dest> <sha256>: download, verify, keep only on a match.
define fetch
	@mkdir -p $(dir $(2))
	@curl -fsSL -o $(2).part $(1)
	@echo "$(3)  $(2).part" | $(SHA256) -c - >/dev/null || { echo "checksum mismatch for $(1)" >&2; rm -f $(2).part; exit 1; }
	@mv $(2).part $(2)
endef

.PHONY: tools
tools: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) $(KUBECONFORM) $(SHELLCHECK) ## Install the pinned test tools into .tools/

# Installed from a verified, extracted copy: works on Helm 3 and 4 and needs
# no signature flags.
$(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION):
	$(call fetch,https://github.com/helm-unittest/helm-unittest/releases/download/v$(HELM_UNITTEST_VERSION)/unittest-$(HELM_UNITTEST_VERSION).tgz,$(TOOLS)/dl/unittest-$(HELM_UNITTEST_VERSION).tgz,$(HELM_UNITTEST_SHA256))
	@rm -rf $(TOOLS)/dl/unittest && tar -xzf $(TOOLS)/dl/unittest-$(HELM_UNITTEST_VERSION).tgz -C $(TOOLS)/dl
	@mkdir -p $(HELM_PLUGINS)
	@$(HELM) plugin uninstall unittest >/dev/null 2>&1 || true
	@echo "==> installing helm-unittest $(HELM_UNITTEST_VERSION) into $(HELM_PLUGINS)"
	@$(HELM) plugin install $(TOOLS)/dl/unittest >/dev/null
	@touch $@

$(KUBECONFORM):
	$(call fetch,https://github.com/yannh/kubeconform/releases/download/v$(KUBECONFORM_VERSION)/kubeconform-$(OS)-$(ARCH).tar.gz,$(TOOLS)/dl/kubeconform-$(KUBECONFORM_VERSION).tgz,$(KUBECONFORM_SHA256_$(OS)_$(ARCH)))
	@mkdir -p $(dir $@) && tar -xzf $(TOOLS)/dl/kubeconform-$(KUBECONFORM_VERSION).tgz -C $(dir $@) kubeconform

$(SHELLCHECK):
	$(call fetch,https://github.com/koalaman/shellcheck/releases/download/v$(SHELLCHECK_VERSION)/shellcheck-v$(SHELLCHECK_VERSION).$(OS).$(SC_ARCH).tar.gz,$(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz,$(SHELLCHECK_SHA256_$(OS)_$(ARCH)))
	@tar -xzf $(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz -C $(TOOLS)

.PHONY: lint
lint: $(SHELLCHECK) ## helm lint --strict (default and example values), shellcheck
	@for c in $(CHARTS); do \
		echo "==> lint $$c"; \
		$(HELM) lint --strict $$c >/dev/null || { $(HELM) lint --strict $$c; exit 1; }; \
		for v in $(CHART_VALUES); do \
			echo "==> lint $$c -f $$v"; \
			$(HELM) lint --strict $$c -f $$v --set storage.hostPath.path=/var/lib/ci-accelerator-lint >/dev/null \
				|| { $(HELM) lint --strict $$c -f $$v --set storage.hostPath.path=/var/lib/ci-accelerator-lint; exit 1; }; \
		done; \
	done
	@echo "==> shellcheck"
	@$(SHELLCHECK) -S warning test/e2e/run.sh

.PHONY: unittest
unittest: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) ## Run helm-unittest suites
	@for c in $(CHARTS); do \
		echo "==> unittest $$c"; \
		$(HELM) unittest --strict $$c || exit 1; \
	done

.PHONY: validate
validate: $(KUBECONFORM) ## Validate rendered manifests against Kubernetes schemas
	@for c in $(CHARTS); do \
		for k in $(KUBE_VERSIONS); do \
			for v in "" $(CHART_VALUES); do \
				echo "==> kubeconform $$c k8s $$k $${v:+-f $$v}"; \
				$(HELM) template test $$c --kube-version $$k $${v:+-f $$v} --set storage.hostPath.path=/var/lib/ci-accelerator-lint \
					| $(KUBECONFORM) -strict -summary -kubernetes-version $$k || exit 1; \
			done; \
		done; \
	done

.PHONY: template
template: ## Render every chart with default values to stdout
	@for c in $(CHARTS); do \
		echo "# ==> $$c"; \
		$(HELM) template test $$c || exit 1; \
	done

.PHONY: kind-up
kind-up: ## Create the kind cluster (if missing) and taint its build nodes
	@kind get clusters 2>/dev/null | grep -qx '$(KIND_CLUSTER)' || \
		kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG) $(if $(KIND_IMAGE),--image $(KIND_IMAGE))
	@kubectl --context $(KUBE_CONTEXT) taint nodes -l ci-accelerator.io/node-role=build \
		ci-accelerator.io/node-role=build:NoSchedule --overwrite >/dev/null
	@kubectl --context $(KUBE_CONTEXT) get nodes -L ci-accelerator.io/node-role,topology.kubernetes.io/zone

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: e2e
e2e: ## End-to-end checks on the kind cluster (KEEP=1 keeps resources, SCENARIOS=... selects)
	@test/e2e/run.sh

.PHONY: e2e-clean
e2e-clean: ## Remove what an interrupted or KEEP=1 e2e run left behind
	@test/e2e/run.sh clean
