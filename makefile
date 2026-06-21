.SUFFIXES:

UV ?= uv
UVX ?= uvx

help:  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "} {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test:  ## Run the engine self-tests
	$(UV) run pytest tests/ -v

.PHONY: lint
lint:  ## ruff check
	$(UVX) ruff check .

.PHONY: format
format:  ## ruff format + ruff check --fix
	$(UVX) ruff format .
	$(UVX) ruff check --fix .

.PHONY: verify
verify: lint test  ## Aggregate gate: lint + test

# ----- sandbox images -----
# matrix machinery kept so it scales, but only 26.04 is wired for now.
DOCKER ?= docker

IMG_VERSIONS := 26.04
IMG_ARCHES   := amd64 arm64

# narrow via VER=.. ARCH=.., e.g. `make images VER=26.04 ARCH=amd64`
VER  ?=
ARCH ?=
SEL_VERS     := $(if $(strip $(VER)),$(VER),$(IMG_VERSIONS))
SEL_ARCHES   := $(if $(strip $(ARCH)),$(ARCH),$(IMG_ARCHES))
SEL_VER_ARCH := $(foreach v,$(SEL_VERS),$(foreach a,$(SEL_ARCHES),$(v)-$(a)))
SANDBOX_STAMPS := $(addprefix .stamp/sandbox-,$(SEL_VER_ARCH))

.stamp:
	@mkdir -p $@

# FORCE recomputes the input hash each run; the stamp (-> rebuild) only moves
# when an input actually changes. heavy lifting lives in hack/build_image.sh.
.PHONY: FORCE
FORCE:

.PHONY: images
images: $(SANDBOX_STAMPS)  ## Build sandbox images (narrow via VER=.. ARCH=..)

.stamp/sandbox-%: FORCE | .stamp
	@hack/build_image.sh sandbox-$*

.PHONY: clean-images
clean-images:  ## Remove image stamps (forces rebuild next run)
	rm -rf .stamp
