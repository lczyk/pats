.SUFFIXES:

SRCS := $(shell find ./cmd ./internal -name '*.go' ! -name 'version.go')

help:  ## Show this help
	@grep -E '^[a-zA-Z_./-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "} {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ./bin/pats  ## Build the pats binary into ./bin (upx-compressed if available)

./bin/pats: $(SRCS) generate-version makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/pats ./cmd/pats
	@if command -v upx >/dev/null 2>&1; then \
		upx ./bin/pats || echo "upx failed, skipping compression"; \
	fi

.PHONY: generate-version
generate-version:  ## Generate internal/version/version.go from VERSION + git
	go run github.com/lczyk/version/go/cmd/generate-version -out ./internal/version/version.go -pkg version

.PHONY: install
install: ./bin/pats  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(PWD)/bin/pats" "$(HOME)/.local/bin/pats"

.PHONY: test
test: generate-version  ## Run the test suite with the race detector
	go test -race ./...

.PHONY: lint
lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l ./cmd ./internal); \
	if [ -n "$$out" ]; then \
		echo "unformatted files:"; echo "$$out"; exit 1; \
	fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w ./cmd ./internal

.PHONY: verify
verify: lint test  ## Aggregate gate: lint + test

.PHONY: clean
clean:  ## Remove build artifacts
	rm -f ./bin/pats

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
