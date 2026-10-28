# One-command targets for every check; CI is meant to run the same targets.
# Works with GNU Make 3.81 (macOS) and 4.x (Linux), Helm 3 and Helm 4.

HELM   ?= helm
CHARTS := charts/buildkit
# The control-plane chart needs its own required values.
CONTROL_PLANE := charts/control-plane
CONTROL_PLANE_SET := --set 'gitlab.urls={https://gitlab.example.com/}' --set daemons.discover=buildkit-headless.ci-accelerator.svc:1234 --set tls.existingSecret=lint-client --set controller.api.tls.existingSecret=lint-api --set jobs.namespace=ci-jobs
CONTROL_PLANE_SIGN_IN_SET := --set router.enabled=true --set signIn.enabled=true --set signIn.url=https://ci.example.com --set signIn.sessionKey.existingSecret=lint-session --set-json 'signIn.gitlab=[{"url":"https://gitlab.example.com/","existingSecret":"lint-oauth","admins":["root"]}]'
CONTROL_PLANE_CERT_MANAGER_SET := --set 'gitlab.urls={https://gitlab.example.com/}' --set daemons.discover=buildkit-headless.ci-accelerator.svc:1234 --set global.tls.issuerRef.name=lint-ca --api-versions cert-manager.io/v1 --set jobs.namespace=ci-jobs --set router.enabled=true --set networkPolicy.enabled=true
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

# Helm, kind and kubectl for CI runners (Linux), where nothing is installed:
# `make ci-tools CI_HELM_VERSION=v3.22.0` puts them into .tools/bin.
# Locally, the installed helm/kind/kubectl are used.
CI_HELM_VERSION ?= v4.3.0
HELM_SHA256_v3.22.0_linux_amd64 := 1e4ab49e429626cf6c6958d914248b78c9730803c2751b87627e171dc800e7bb
HELM_SHA256_v3.22.0_linux_arm64 := f14e804dfee240f55525b667488fe9adca349e63e00c9af634c0beb1421ac310
HELM_SHA256_v4.3.0_linux_amd64  := 86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb
HELM_SHA256_v4.3.0_linux_arm64  := 31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08
KIND_VERSION := v0.33.0
KIND_SHA256_linux_amd64 := aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
KIND_SHA256_linux_arm64 := 20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
KUBECTL_VERSION := v1.37.1
KUBECTL_SHA256_linux_amd64 := 65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
KUBECTL_SHA256_linux_arm64 := ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7
# Vulnerability scanners: govulncheck (golang.org/x/vuln, installed with Go,
# verified by the Go checksum database) for the Go code, trivy for
# its image. Checksums from trivy's <version>_checksums.txt release asset.
GOVULNCHECK_VERSION := 1.8.0
# Chart READMEs from values comments (helm-docs, installed with Go) and the
# Artifact Hub metadata push (oras; checksums from its release's checksums file).
HELM_DOCS_VERSION := 1.14.2
ORAS_VERSION := 1.3.4
ORAS_SHA256_linux_amd64  := f27adb935022d94df8dc77719c322dda592c78a0d57a6f7dcdd8d900b248c454
ORAS_SHA256_linux_arm64  := 15702c6e3a4a56a8bd8ac5c17efdbcab56d9bada661ccbcf017f5b10c1d89399
ORAS_SHA256_darwin_arm64 := 217761a9500242ff473de8656b5aca21136ff39e17e9e61fd8936bbfd902704c
TRIVY_VERSION := 0.75.0
TRIVY_SHA256_linux_amd64  := c6e65abddb348e25f10549df887045629cf28cc72453cd1c63acb717316b3f3f
TRIVY_SHA256_linux_arm64  := a1ee9f6ffb7d112b64ff726a2a0717c21175c1114361391f4a132956751a13b3
TRIVY_SHA256_darwin_arm64 := 4a77108cccf8e55c8d6823e1e759939a622277e66cd0daa3c1fc621ed69e4568
TRIVY_OS = $(subst linux,Linux,$(subst darwin,macOS,$(OS)))
TRIVY_ARCH = $(subst amd64,64bit,$(subst arm64,ARM64,$(ARCH)))
# Go for the control plane (internal/, cmd/). Locally any Go >= 1.21 fetches this
# toolchain itself (GOTOOLCHAIN, verified against the Go checksum database);
# CI downloads the archive below. Checksums from https://go.dev/dl/?mode=json.
GO_VERSION := 1.27.3
GO_SHA256_linux_amd64 := 65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
GO_SHA256_linux_arm64 := a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
# Node for the browser checks (test/ui): LTS line 24, the newest release at
# least two weeks old; sums from nodejs.org SHASUMS256.txt.
NODE_VERSION := 24.22.1
NODE_SHA256_linux_amd64  := 6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6
NODE_SHA256_linux_arm64  := dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e
NODE_SHA256_darwin_arm64 := 654ba79de6155647e22321932b45f2a1ccb705976cc337c9b8e278beb085a628
NODE_SHA256_darwin_amd64 := 65e450f90a6eaacd5000f611861c4ce65fadc87f813795dc4ed0c0e5f2fca2d4

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
CI_BIN      := $(TOOLS)/bin
CI_HELM     := $(TOOLS)/helm-$(CI_HELM_VERSION)/helm
CI_KIND     := $(TOOLS)/kind-$(KIND_VERSION)/kind
CI_KUBECTL  := $(TOOLS)/kubectl-$(KUBECTL_VERSION)/kubectl
CI_GO       := $(TOOLS)/go-$(GO_VERSION)/bin/go
NODE        := $(TOOLS)/node-v$(NODE_VERSION)/bin/node
GO          ?= go
GO_RUN      := GOTOOLCHAIN=go$(GO_VERSION) $(GO)

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

# fetch <url> <dest> <sha256>: download (retried: CI runners see dropped
# connections), verify, keep only on a match.
define fetch
	@mkdir -p $(dir $(2))
	@curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors -o $(2).part $(1)
	@echo "$(3)  $(2).part" | $(SHA256) -c - >/dev/null || { echo "checksum mismatch for $(1)" >&2; rm -f $(2).part; exit 1; }
	@mv $(2).part $(2)
endef

.PHONY: tools
tools: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) $(KUBECONFORM) $(SHELLCHECK) $(RUNNER_CHART) ## Install the pinned test tools and charts into .tools/

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

GOVULNCHECK := $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)/govulncheck
TRIVY       := $(TOOLS)/trivy-$(TRIVY_VERSION)/trivy
HELM_DOCS   := $(TOOLS)/helm-docs-$(HELM_DOCS_VERSION)/helm-docs
ORAS        := $(TOOLS)/oras-$(ORAS_VERSION)/oras

$(HELM_DOCS):
	@mkdir -p $(dir $@)
	GOBIN=$(dir $@) $(GO_RUN) install github.com/norwoodj/helm-docs/cmd/helm-docs@v$(HELM_DOCS_VERSION)

$(ORAS):
	$(call fetch,https://github.com/oras-project/oras/releases/download/v$(ORAS_VERSION)/oras_$(ORAS_VERSION)_$(OS)_$(ARCH).tar.gz,$(TOOLS)/dl/oras-$(ORAS_VERSION).tgz,$(ORAS_SHA256_$(OS)_$(ARCH)))
	@mkdir -p $(dir $@) && tar -xzf $(TOOLS)/dl/oras-$(ORAS_VERSION).tgz -C $(dir $@) oras

.PHONY: oras
oras: $(ORAS) ## Install the pinned oras into .tools (the workflows log in to ghcr with it)
	@echo "$(ORAS)"

.PHONY: chart-docs chart-docs-check
chart-docs: $(HELM_DOCS) ## Regenerate charts/*/README.md from Chart.yaml, values comments and README.md.gotmpl
	@$(HELM_DOCS) --chart-search-root charts --log-level warning

chart-docs-check: $(HELM_DOCS) ## Fail when a chart README is out of date (make chart-docs)
	@$(HELM_DOCS) --chart-search-root charts --log-level warning --output-file README.generated.md
	@rc=0; for c in charts/*/; do \
		cmp -s $$c/README.md $$c/README.generated.md || { echo "$$c/README.md is out of date: run make chart-docs"; rc=1; }; \
		rm -f $$c/README.generated.md; done; exit $$rc

# Pushes each chart's Artifact Hub repository metadata (artifacthub/<chart>.yml:
# its repository id and owners) to the tag artifacthub.io of its OCI
# repository, as Artifact Hub reads it. The file must be named
# artifacthub-repo.yml in the layer.
.PHONY: artifacthub-metadata
artifacthub-metadata: $(ORAS) ## Push artifacthub/<chart>.yml to every chart's OCI repository (CHARTS_REPO=ghcr.io/<owner>/charts)
	@[ -n "$(CHARTS_REPO)" ] || { echo "set CHARTS_REPO=ghcr.io/<owner>/charts" >&2; exit 2; }
	@for c in buildkit control-plane ci-accelerator; do \
		rm -rf $(TOOLS)/artifacthub && mkdir -p $(TOOLS)/artifacthub && cp artifacthub/$$c.yml $(TOOLS)/artifacthub/artifacthub-repo.yml && \
		(cd $(TOOLS)/artifacthub && $(ORAS) push "$(CHARTS_REPO)/$$c:artifacthub.io" \
			--config /dev/null:application/vnd.cncf.artifacthub.config.v1+yaml \
			artifacthub-repo.yml:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml >/dev/null) \
			&& echo "pushed $(CHARTS_REPO)/$$c:artifacthub.io"; done

$(GOVULNCHECK):
	@mkdir -p $(dir $@)
	GOBIN=$(dir $@) $(GO_RUN) install golang.org/x/vuln/cmd/govulncheck@v$(GOVULNCHECK_VERSION)

$(TRIVY):
	$(call fetch,https://github.com/aquasecurity/trivy/releases/download/v$(TRIVY_VERSION)/trivy_$(TRIVY_VERSION)_$(TRIVY_OS)-$(TRIVY_ARCH).tar.gz,$(TOOLS)/dl/trivy-$(TRIVY_VERSION).tgz,$(TRIVY_SHA256_$(OS)_$(ARCH)))
	@mkdir -p $(dir $@) && tar -xzf $(TOOLS)/dl/trivy-$(TRIVY_VERSION).tgz -C $(dir $@) trivy

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Known vulnerabilities reachable from the Go code, both release targets (.govulncheck-allow for grace periods)
	@GOVULNCHECK=$(GOVULNCHECK) GOTOOLCHAIN=go$(GO_VERSION) scripts/vulncheck.sh

# IMAGE=<ref> scans a registry image, IMAGE_TAR=<docker archive> a local one
# (scripts/build-image.sh writes it with SCAN_TAR). Fails on HIGH or
# CRITICAL findings that have a fix; .trivyignore.yaml holds grace periods.
.PHONY: image-scan
image-scan: $(TRIVY) ## trivy over the ci-accelerator image (IMAGE=<ref> or IMAGE_TAR=<file>): HIGH/CRITICAL with a fix fail
	@[ -n "$(IMAGE)$(IMAGE_TAR)" ] || { echo "set IMAGE=<ref> or IMAGE_TAR=<docker archive>" >&2; exit 2; }
	$(TRIVY) image --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 \
		--ignorefile .trivyignore.yaml $(if $(IMAGE_TAR),--input $(IMAGE_TAR),$(IMAGE))

.PHONY: ci-tools
ci-tools: $(CI_HELM) $(CI_KIND) $(CI_KUBECTL) $(CI_GO) ## CI only (Linux): pinned helm (CI_HELM_VERSION), kind, kubectl and Go into .tools/bin
	@mkdir -p $(CI_BIN)
	@cp $(CI_HELM) $(CI_KIND) $(CI_KUBECTL) $(CI_BIN)/
	@ln -sf $(CI_GO) $(CI_BIN)/go
	@echo "==> .tools/bin: $$($(CI_BIN)/helm version --short), kind $(KIND_VERSION), kubectl $(KUBECTL_VERSION), $$($(CI_GO) version)"

.PHONY: go-tools
go-tools: $(CI_GO) ## CI only (Linux): pinned Go into .tools/go-<version>

$(CI_GO):
	@test "$(OS)" = linux || { echo "ci-tools is for Linux CI runners; any local Go >= 1.21 fetches go$(GO_VERSION) itself" >&2; exit 1; }
	$(call fetch,https://go.dev/dl/go$(GO_VERSION).linux-$(ARCH).tar.gz,$(TOOLS)/dl/go-$(GO_VERSION).tgz,$(GO_SHA256_linux_$(ARCH)))
	@mkdir -p $(TOOLS)/go-$(GO_VERSION) && tar -xzf $(TOOLS)/dl/go-$(GO_VERSION).tgz -C $(TOOLS)/go-$(GO_VERSION) --strip-components=1

$(CI_HELM):
	@test "$(OS)" = linux || { echo "ci-tools is for Linux CI runners; install helm locally" >&2; exit 1; }
	$(call fetch,https://get.helm.sh/helm-$(CI_HELM_VERSION)-linux-$(ARCH).tar.gz,$(TOOLS)/dl/helm-$(CI_HELM_VERSION).tgz,$(HELM_SHA256_$(CI_HELM_VERSION)_linux_$(ARCH)))
	@mkdir -p $(dir $@) && tar -xzf $(TOOLS)/dl/helm-$(CI_HELM_VERSION).tgz -C $(dir $@) --strip-components=1 linux-$(ARCH)/helm

$(CI_KIND):
	@test "$(OS)" = linux || { echo "ci-tools is for Linux CI runners; install kind locally" >&2; exit 1; }
	$(call fetch,https://github.com/kubernetes-sigs/kind/releases/download/$(KIND_VERSION)/kind-linux-$(ARCH),$@,$(KIND_SHA256_linux_$(ARCH)))
	@chmod +x $@

$(CI_KUBECTL):
	@test "$(OS)" = linux || { echo "ci-tools is for Linux CI runners; install kubectl locally" >&2; exit 1; }
	$(call fetch,https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/linux/$(ARCH)/kubectl,$@,$(KUBECTL_SHA256_linux_$(ARCH)))
	@chmod +x $@

$(SHELLCHECK):
	$(call fetch,https://github.com/koalaman/shellcheck/releases/download/v$(SHELLCHECK_VERSION)/shellcheck-v$(SHELLCHECK_VERSION).$(OS).$(SC_ARCH).tar.gz,$(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz,$(SHELLCHECK_SHA256_$(OS)_$(ARCH)))
	@tar -xzf $(TOOLS)/dl/shellcheck-$(SHELLCHECK_VERSION).tgz -C $(TOOLS)

# The umbrella chart vendors its dependencies: buildkit packaged from this
# repository, the others from checksum-verified archives.
UMBRELLA := charts/ci-accelerator

.PHONY: deps
deps: $(RUNNER_CHART) ## Vendor the umbrella chart's dependencies into charts/ci-accelerator/charts/
	@mkdir -p $(UMBRELLA)/charts && rm -f $(UMBRELLA)/charts/*.tgz
	@$(HELM) package charts/buildkit -d $(UMBRELLA)/charts >/dev/null
	@$(HELM) package $(CONTROL_PLANE) -d $(UMBRELLA)/charts >/dev/null
	@cp $(RUNNER_CHART) $(UMBRELLA)/charts/

.PHONY: lint
lint: $(SHELLCHECK) deps chart-docs-check ## helm lint --strict (default and example values), chart LICENSE copies, READMEs up to date, shellcheck
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
	@echo "==> lint $(CONTROL_PLANE)"
	@cmp -s LICENSE $(CONTROL_PLANE)/LICENSE || { echo "$(CONTROL_PLANE)/LICENSE differs from LICENSE"; exit 1; }
	@$(HELM) lint --strict $(CONTROL_PLANE) $(CONTROL_PLANE_SET) >/dev/null || { $(HELM) lint --strict $(CONTROL_PLANE) $(CONTROL_PLANE_SET); exit 1; }
	@$(HELM) lint --strict $(CONTROL_PLANE) $(CONTROL_PLANE_SET) $(CONTROL_PLANE_SIGN_IN_SET) >/dev/null || { $(HELM) lint --strict $(CONTROL_PLANE) $(CONTROL_PLANE_SET) $(CONTROL_PLANE_SIGN_IN_SET); exit 1; }
	@echo "==> lint $(UMBRELLA)"
	@cmp -s LICENSE $(UMBRELLA)/LICENSE || { echo "$(UMBRELLA)/LICENSE differs from LICENSE"; exit 1; }
	@$(HELM) lint --strict $(UMBRELLA) -f $(UMBRELLA)/tests/values/lint.yaml >/dev/null \
		|| { $(HELM) lint --strict $(UMBRELLA) -f $(UMBRELLA)/tests/values/lint.yaml; exit 1; }
	@echo "==> shellcheck"
	@$(SHELLCHECK) -S warning test/e2e/run.sh scripts/buildkit-certs.sh scripts/check-release-version.sh scripts/changelog-section.sh test/runner-config-parity.sh test/e2e-matrix.sh scripts/build-image.sh scripts/quickstart.sh test/quickstart/run.sh scripts/vulncheck.sh

.PHONY: unittest
unittest: $(HELM_PLUGINS)/.unittest-$(HELM_UNITTEST_VERSION) deps ## Run helm-unittest suites
	@for c in $(CHARTS) $(CONTROL_PLANE) $(UMBRELLA); do \
		echo "==> unittest $$c"; \
		$(HELM) unittest --strict $$c || exit 1; \
	done

# The ci-accelerator image (controller, web, disk probe) for kind and the
# e2e (not published from here).
LOCAL_IMAGE ?= ci-accelerator:dev
.PHONY: image
image: ## Build the ci-accelerator image for this machine's architecture and load it into kind
	@docker buildx build -f cmd/ci-accelerator/Dockerfile -t $(LOCAL_IMAGE) --build-arg VERSION=dev --build-arg COMMIT=$${COMMIT:-$$(git rev-parse --short=12 HEAD)} --load . >/dev/null
	@kind load docker-image $(LOCAL_IMAGE) --name $(KIND_CLUSTER) >/dev/null 2>&1
	@echo "$(LOCAL_IMAGE) loaded into kind cluster $(KIND_CLUSTER)"

.PHONY: ui-tools
ui-tools: $(NODE) ## Pinned Node for the browser checks into .tools/node-v<version>

$(NODE):
	$(call fetch,https://nodejs.org/dist/v$(NODE_VERSION)/node-v$(NODE_VERSION)-$(OS)-$(if $(filter amd64,$(ARCH)),x64,arm64).tar.xz,$(TOOLS)/dl/node-$(NODE_VERSION).txz,$(NODE_SHA256_$(OS)_$(ARCH)))
	@mkdir -p $(TOOLS)/node-v$(NODE_VERSION) && tar -xJf $(TOOLS)/dl/node-$(NODE_VERSION).txz -C $(TOOLS)/node-v$(NODE_VERSION) --strip-components=1

.PHONY: component-test
component-test: ## The GitLab CI component's job script against a fake buildctl, in its own image (needs docker, ruby)
	@ci/gitlab/test/run.sh

.PHONY: ui-test
ui-test: $(NODE) ## Browser checks of the console's pages on its fixtures (needs Chrome; CHROME=<path> to pick one)
	@UI_TEST=1 NODE=$(NODE) $(GO_RUN) test -count=1 -v -run '^TestUI(SignedIn)?$$' -timeout 15m ./internal/web/
	@echo "==> the same pages through the controller's API (controller and web apart)"
	@UI_TEST=1 WEB_TEST_SPLIT=1 NODE=$(NODE) $(GO_RUN) test -count=1 -v -run '^TestUI(SignedIn)?$$' -timeout 15m ./internal/web/

.PHONY: ui-shots
ui-shots: $(NODE) ## Screenshots of the console's pages on its fixtures at 1400 and 390 px into .ui-shots/ (open its index.html)
	@rm -rf .ui-shots && UI_TEST=1 UI_SHOTS=$(CURDIR)/.ui-shots NODE=$(NODE) $(GO_RUN) test -count=1 -run '^TestUI$$' -timeout 15m ./internal/web/ && \
		echo "==> .ui-shots/TestUI/index.html"

.PHONY: console-dev
console-dev: ## The console on its fixtures at CONSOLE_DEV (127.0.0.1:8095), pages read from internal/web on every request
	@CONSOLE_DEV=$${CONSOLE_DEV:-127.0.0.1:8095} $(GO_RUN) test -count=1 -v -run '^TestDevServer$$' -timeout 0 ./internal/web/

.PHONY: go-test
go-test: ## Go: gofmt, go vet and the tests (the pages twice: with the controller in the process, and through its API)
	@out=$$("$$($(GO_RUN) env GOROOT)/bin/gofmt" -l internal cmd); \
		if [ -n "$$out" ]; then echo "not gofmt-formatted: $$out" >&2; exit 1; fi
	@$(GO_RUN) vet ./...
	@$(GO_RUN) test -count=1 ./...
	@WEB_TEST_SPLIT=1 $(GO_RUN) test -count=1 ./internal/web/

.PHONY: quick
quick: lint unittest go-test ## The checks to run before every push (validate, e2e and the browser checks run in CI)

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
		echo "==> kubeconform $(CONTROL_PLANE) k8s $$k"; \
		$(HELM) template test $(CONTROL_PLANE) --kube-version $$k -n ci-accelerator $(CONTROL_PLANE_SET) \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k || exit 1; \
		echo "==> kubeconform $(CONTROL_PLANE) k8s $$k sign-in"; \
		$(HELM) template test $(CONTROL_PLANE) --kube-version $$k -n ci-accelerator $(CONTROL_PLANE_SET) $(CONTROL_PLANE_SIGN_IN_SET) \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k || exit 1; \
		echo "==> kubeconform $(CONTROL_PLANE) k8s $$k cert-manager"; \
		$(HELM) template test $(CONTROL_PLANE) --kube-version $$k -n ci-accelerator $(CONTROL_PLANE_CERT_MANAGER_SET) \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k -skip $(CERT_MANAGER_KINDS) || exit 1; \
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
		echo "==> kubeconform $(UMBRELLA) k8s $$k runner cache and tiers"; \
		$(HELM) template ci-accelerator $(UMBRELLA) --kube-version $$k -n ci-accelerator --api-versions cert-manager.io/v1 \
			-f $(UMBRELLA)/tests/values/required.yaml -f $(UMBRELLA)/tests/values/tiers.yaml \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k -skip $(CERT_MANAGER_KINDS) || exit 1; \
		echo "==> kubeconform $(UMBRELLA) k8s $$k control plane"; \
		$(HELM) template ci-accelerator $(UMBRELLA) --kube-version $$k -n ci-accelerator --api-versions cert-manager.io/v1 \
			-f $(UMBRELLA)/tests/values/required.yaml -f $(UMBRELLA)/tests/values/control-plane.yaml \
			| $(KUBECONFORM) -strict -summary -kubernetes-version $$k -skip $(CERT_MANAGER_KINDS) || exit 1; \
	done
	@test/runner-config-parity.sh $(HELM) $(RUNNER_CHART)
	@test/e2e-matrix.sh

.PHONY: template
template: deps ## Render every chart with default values (plus the required ones) to stdout
	@for c in $(CHARTS); do \
		echo "# ==> $$c"; \
		$(HELM) template test $$c $(RENDER_SET) || exit 1; \
	done
	@echo "# ==> $(CONTROL_PLANE)"
	@$(HELM) template test $(CONTROL_PLANE) $(CONTROL_PLANE_SET)
	@echo "# ==> $(UMBRELLA)"
	@$(HELM) template test $(UMBRELLA) -f $(UMBRELLA)/tests/values/lint.yaml

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

.PHONY: e2e-copy
e2e-copy: ## The e2e on a copy of the tree taken now, so the tree can change while it runs (SCENARIOS=... selects)
	@d=$$(mktemp -d "$${TMPDIR:-/tmp}/ci-accelerator-e2e.XXXXXX"); \
		rsync -a --exclude .git --exclude .tools ./ "$$d/" && ln -s "$(CURDIR)/.tools" "$$d/.tools" && \
		echo "==> e2e on a copy in $$d" && COMMIT=$$(git rev-parse --short=12 HEAD) $(MAKE) -C "$$d" e2e; rc=$$?; rm -rf "$$d"; exit $$rc

.PHONY: e2e-clean
e2e-clean: ## Remove what an interrupted or KEEP=1 e2e run left behind
	@test/e2e/run.sh clean
