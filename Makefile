# One-command targets for every check; CI is meant to run the same targets.
# Works with GNU Make 3.81 (macOS) and 4.x (Linux), Helm 3 and Helm 4.

HELM   ?= helm
CHARTS := charts/buildkit
# Extra values files every chart check also renders with.
CHART_VALUES := examples/kind/values.yaml test/e2e/values.yaml
# Values the charts require and the files above leave to the installer.
# helm lint cannot fake the cert-manager API, so checks render the
# existingSecret mode; validate renders the cert-manager mode as well.
RENDER_SET := --set tls.source=existingSecret --set tls.existingSecret=lint-tls --set storage.hostPath.path=/var/lib/ci-accelerator-lint --set networkPolicy.ingress.namespaces={ci-jobs}
CERT_MANAGER_SET := --api-versions cert-manager.io/v1 --set storage.hostPath.path=/var/lib/ci-accelerator-lint --set tls.client.namespaces={ci-jobs}
# kubeconform has no schemas for cert-manager's kinds; the e2e checks them
# against the real CRDs.
CERT_MANAGER_KINDS := Certificate,Issuer,ClusterIssuer
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

# The official gitlab-runner chart integrations/gitlab-runner/values.yaml is
# written for; make validate renders it with the fragment.
RUNNER_CHART_VERSION := 0.93.0
RUNNER_CHART_SHA256  := a8932d04c94724aa93e3147811696e3512df9cbaa5901818d2b3872b0f02f0a9

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
RUNNER_CHART := $(TOOLS)/charts/gitlab-runner-$(RUNNER_CHART_VERSION).tgz

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
tools: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) $(KUBECONFORM) $(SHELLCHECK) $(RUNNER_CHART) ## Install the pinned test tools into .tools/

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

$(RUNNER_CHART):
	$(call fetch,https://gitlab-charts.s3.amazonaws.com/gitlab-runner-$(RUNNER_CHART_VERSION).tgz,$@,$(RUNNER_CHART_SHA256))

$(SHELLCHECK):
	$(call fetch,https://github.com/koalaman/shellcheck/releases/download/v$(SHELLCHECK_VERSION)/shellcheck-v$(SHELLCHECK_VERSION).$(OS).$(SC_ARCH).tar.gz,$(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz,$(SHELLCHECK_SHA256_$(OS)_$(ARCH)))
	@tar -xzf $(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz -C $(TOOLS)

# The umbrella chart vendors its dependencies: buildkit packaged from this
# repository, gitlab-runner from the checksum-verified archive.
UMBRELLA := charts/ci-accelerator

.PHONY: deps
deps: $(RUNNER_CHART) ## Vendor the umbrella chart's dependencies into charts/ci-accelerator/charts/
	@mkdir -p $(UMBRELLA)/charts && rm -f $(UMBRELLA)/charts/*.tgz
	@$(HELM) package charts/buildkit -d $(UMBRELLA)/charts >/dev/null
	@cp $(RUNNER_CHART) $(UMBRELLA)/charts/

.PHONY: lint
lint: $(SHELLCHECK) deps ## helm lint --strict (default and example values), chart LICENSE copies, shellcheck
	@for c in $(CHARTS); do \
		echo "==> lint $$c"; \
		cmp -s LICENSE $$c/LICENSE || { echo "$$c/LICENSE differs from LICENSE (the packaged chart must carry the license text)"; exit 1; }; \
		$(HELM) lint --strict $$c $(RENDER_SET) >/dev/null || { $(HELM) lint --strict $$c $(RENDER_SET); exit 1; }; \
		for v in $(CHART_VALUES); do \
			echo "==> lint $$c -f $$v"; \
			$(HELM) lint --strict $$c -f $$v $(RENDER_SET) >/dev/null \
				|| { $(HELM) lint --strict $$c -f $$v $(RENDER_SET); exit 1; }; \
		done; \
	done
	@echo "==> lint $(UMBRELLA)"
	@cmp -s LICENSE $(UMBRELLA)/LICENSE || { echo "$(UMBRELLA)/LICENSE differs from LICENSE"; exit 1; }
	@$(HELM) lint --strict $(UMBRELLA) -f $(UMBRELLA)/tests/values/lint.yaml >/dev/null \
		|| { $(HELM) lint --strict $(UMBRELLA) -f $(UMBRELLA)/tests/values/lint.yaml; exit 1; }
	@echo "==> shellcheck"
	@$(SHELLCHECK) -S warning test/e2e/run.sh scripts/buildkit-certs.sh test/runner-config-parity.sh

.PHONY: unittest
unittest: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) deps ## Run helm-unittest suites
	@for c in $(CHARTS) $(UMBRELLA); do \
		echo "==> unittest $$c"; \
		$(HELM) unittest --strict $$c || exit 1; \
	done

.PHONY: validate
validate: $(KUBECONFORM) $(RUNNER_CHART) deps ## Validate rendered manifests (our charts, gitlab-runner + fragment) against Kubernetes schemas
	@for c in $(CHARTS); do \
		for k in $(KUBE_VERSIONS); do \
			for v in "" $(CHART_VALUES); do \
				echo "==> kubeconform $$c k8s $$k $${v:+-f $$v}"; \
				$(HELM) template test $$c --kube-version $$k $${v:+-f $$v} $(RENDER_SET) \
					| $(KUBECONFORM) -strict -summary -kubernetes-version $$k || exit 1; \
			done; \
			echo "==> kubeconform $$c k8s $$k tls.source=certManager"; \
			$(HELM) template test $$c --kube-version $$k $(CERT_MANAGER_SET) \
				| $(KUBECONFORM) -strict -summary -kubernetes-version $$k -skip $(CERT_MANAGER_KINDS) || exit 1; \
		done; \
	done
	@for k in $(KUBE_VERSIONS); do \
		echo "==> kubeconform gitlab-runner $(RUNNER_CHART_VERSION) + integrations/gitlab-runner k8s $$k"; \
		$(HELM) template gitlab-runner $(RUNNER_CHART) --kube-version $$k -n ci-accelerator \
			-f integrations/gitlab-runner/values.yaml \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k || exit 1; \
		echo "==> kubeconform $(UMBRELLA) k8s $$k"; \
		$(HELM) template ci-accelerator $(UMBRELLA) --kube-version $$k -n ci-accelerator --api-versions cert-manager.io/v1 \
			-f $(UMBRELLA)/tests/values/required.yaml \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k -skip $(CERT_MANAGER_KINDS) || exit 1; \
	done
	@test/runner-config-parity.sh $(HELM) $(RUNNER_CHART)

.PHONY: template
template: ## Render every chart with default values (plus the required ones) to stdout
	@for c in $(CHARTS); do \
		echo "# ==> $$c"; \
		$(HELM) template test $$c $(RENDER_SET) || exit 1; \
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
