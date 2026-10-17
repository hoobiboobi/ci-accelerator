# One-command targets for every check. The same targets run in GitHub Actions.
# Kept compatible with GNU Make 3.81 (the version shipped with macOS).

HELM   ?= helm
CHARTS := charts/buildkit

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: lint
lint: ## helm lint --strict every chart
	@for c in $(CHARTS); do \
		echo "==> lint $$c"; \
		$(HELM) lint --strict $$c || exit 1; \
	done

.PHONY: template
template: ## Render every chart with default values to stdout
	@for c in $(CHARTS); do \
		echo "# ==> $$c"; \
		$(HELM) template test $$c || exit 1; \
	done
