SHELL := /bin/bash
.DEFAULT_GOAL := help

# ---- Configuration (override on command line, e.g. `make deploy SPACE=dev`) ----

IMAGE_TAG     ?= dev
IMAGE         := ghcr.io/jesperfj/ghfn-worker:$(IMAGE_TAG)
NAMESPACE     ?= default
KIND_CLUSTER  ?= ghfn
RELEASE       ?= ghfn

# ConfigHub-side
SPACE         ?= default
WORKER_SLUG   ?= ghfn
CONFIGHUB_URL ?= https://hub.confighub.com

# Source repo for the functions/ tree the worker will clone.
REPO_URL      ?= https://github.com/jesperfj/ghfn-test-functions
REPO_BRANCH   ?= main

# The chart names its resources `<release>-<chart>`. Mirror that here.
FULLNAME = $(RELEASE)-ghfn-worker
SECRET   = $(FULLNAME)-secret

# Local rendered output (gitignored).
RENDERED := .rendered
MANIFEST := $(RENDERED)/manifest.yaml

# Helper: select the running worker pod (re-evaluated each call).
POD = $$(kubectl get pod -n $(NAMESPACE) -l app.kubernetes.io/instance=$(RELEASE) -o jsonpath='{.items[0].metadata.name}')

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---- Build ----

.PHONY: build
build: ## Compile the worker binary locally
	go build ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: image
image: ## Build the worker container image
	docker build -t $(IMAGE) .

.PHONY: kind-load
kind-load: image ## Build and load the image into the kind cluster
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

# ---- ConfigHub: register worker, materialize Secret ----

.PHONY: worker-create
worker-create: ## Register the worker in ConfigHub (idempotent)
	@if cub worker get --space $(SPACE) $(WORKER_SLUG) >/dev/null 2>&1; then \
		echo ">> worker $(WORKER_SLUG) already exists in space $(SPACE)"; \
	else \
		echo ">> creating worker $(WORKER_SLUG) in space $(SPACE)"; \
		cub worker create --space $(SPACE) $(WORKER_SLUG); \
	fi

.PHONY: secret
secret: worker-create ## Apply the worker Secret under the chart's expected name
	# `cub worker install --export-secret-only` outputs a Secret named
	# `confighub-worker-secret`. The chart references $(SECRET); using the cub
	# default would also collide with cub-lk's standard worker Secret in the
	# same namespace (visible "dual connection" symptom). Rename.
	cub worker install --export-secret-only --space $(SPACE) $(WORKER_SLUG) \
		| sed 's/name: confighub-worker-secret/name: $(SECRET)/' \
		| kubectl apply -n $(NAMESPACE) -f -

# ---- Render + post-render edits + apply (mirrors the ConfigHub flow) ----
#
# `helm template` produces the same manifest ConfigHub would import. We then
# patch CONFIGHUB_WORKER_ID and REPO_URL with yq, mirroring the
# `cub function do set-string-path` calls a production user makes.

.PHONY: render
render: ## Render the chart and patch in local-dev values
	@mkdir -p $(RENDERED)
	helm template $(RELEASE) chart/ghfn-worker \
		--namespace $(NAMESPACE) \
		--set image.tag=$(IMAGE_TAG) \
		--set image.pullPolicy=Never \
		> $(MANIFEST)
	@WID=$$(cub worker get --space $(SPACE) $(WORKER_SLUG) -o json | jq -r .BridgeWorker.BridgeWorkerID); \
		echo ">> patching CONFIGHUB_WORKER_ID=$$WID, REPO_URL=$(REPO_URL), REPO_BRANCH=$(REPO_BRANCH)"; \
		WID="$$WID" yq -i '(.spec.template.spec.containers[0].env[] | select(.name == "CONFIGHUB_WORKER_ID").value) = strenv(WID)' $(MANIFEST); \
		REPO_URL="$(REPO_URL)" yq -i '(.spec.template.spec.containers[0].env[] | select(.name == "REPO_URL").value) = strenv(REPO_URL)' $(MANIFEST); \
		REPO_BRANCH="$(REPO_BRANCH)" yq -i '(.spec.template.spec.containers[0].env[] | select(.name == "REPO_BRANCH").value) = strenv(REPO_BRANCH)' $(MANIFEST)
	@echo ">> rendered manifest at $(MANIFEST)"

.PHONY: deploy
deploy: kind-load secret render ## Build, register, and apply the rendered manifest
	kubectl apply -n $(NAMESPACE) -f $(MANIFEST)
	kubectl rollout status -n $(NAMESPACE) deployment/$(FULLNAME) --timeout=120s

# ---- Local dev loop ----

.PHONY: refresh
refresh: ## SIGHUP the worker to reload script bodies (body changes only)
	@P=$(POD) && echo ">> SIGHUP $$P" && \
		kubectl exec -n $(NAMESPACE) $$P -- /bin/sh -c 'kill -HUP 1'

.PHONY: restart
restart: ## Restart the worker pod
	kubectl rollout restart -n $(NAMESPACE) deployment/$(FULLNAME)
	kubectl rollout status -n $(NAMESPACE) deployment/$(FULLNAME) --timeout=120s

.PHONY: logs
logs: ## Tail worker logs
	kubectl logs -n $(NAMESPACE) -l app.kubernetes.io/instance=$(RELEASE) -f

.PHONY: shell
shell: ## Open a shell in the running pod (useful for poking the function dir)
	@P=$(POD) && kubectl exec -n $(NAMESPACE) -it $$P -- /bin/bash

# ---- Teardown ----

.PHONY: undeploy
undeploy: ## Delete the rendered manifest from the cluster + the worker Secret
	-kubectl delete -n $(NAMESPACE) -f $(MANIFEST)
	-kubectl delete -n $(NAMESPACE) secret $(SECRET)

.PHONY: worker-delete
worker-delete: ## Delete the worker entity in ConfigHub
	-cub worker delete --space $(SPACE) $(WORKER_SLUG)

.PHONY: clean
clean: ## Remove the local rendered manifest
	rm -rf $(RENDERED)

# ---- One-shot convenience ----

.PHONY: up
up: deploy ## Bring everything up

.PHONY: cluster-up
cluster-up: ## Bring up the kind cluster
	cub lk up --name $(KIND_CLUSTER)

.PHONY: cluster-down
cluster-down: ## Tear down the kind cluster
	cub lk down --name $(KIND_CLUSTER)

.PHONY: down
down: undeploy worker-delete ## Tear everything down
