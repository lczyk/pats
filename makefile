.SUFFIXES:

SRCS := $(shell find ./cmd ./internal ./src -name '*.go')

help:  ## Show this help
	@grep -E '^[a-zA-Z_./-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "} {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ./bin/pats  ## Build the pats binary into ./bin (upx-compressed if available)

./bin/pats: $(SRCS) makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/pats ./cmd/pats
	@if command -v upx >/dev/null 2>&1; then \
		upx ./bin/pats || echo "upx failed, skipping compression"; \
	fi

.PHONY: install
install: ./bin/pats  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(PWD)/bin/pats" "$(HOME)/.local/bin/pats"

.PHONY: test
test:  ## Run the test suite with the race detector
	go test -race ./...

.PHONY: lint
lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l ./cmd ./internal ./src); \
	if [ -n "$$out" ]; then \
		echo "unformatted files:"; echo "$$out"; exit 1; \
	fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w ./cmd ./internal ./src

.PHONY: cover
cover:  ## Show test coverage per function in the CLI
	go test -coverprofile=/tmp/pats-cover.out ./...
	go tool cover -func=/tmp/pats-cover.out

FUZZTIME ?= 10s

.PHONY: fuzz
fuzz:  ## Run all fuzz targets (narrow via FUZZTIME=..)
	go test ./internal/config -run - -fuzz FuzzParse -fuzztime $(FUZZTIME)
	go test ./internal/eval -run - -fuzz FuzzParseScore -fuzztime $(FUZZTIME)
	go test ./src/sandbox/proxy -run - -fuzz FuzzPermitsURL -fuzztime $(FUZZTIME)
	go test ./src/sandbox/proxy -run - -fuzz FuzzParseURLRules -fuzztime $(FUZZTIME)

.PHONY: bench
bench:  ## Run all benchmarks
	go test ./... -run - -bench . -benchmem

.PHONY: verify
verify: lint test  ## Aggregate gate: lint + test

.PHONY: clean
clean:  ## Remove build artifacts
	rm -f ./bin/pats

# ----- sandbox images -----
# matrix machinery kept so it scales, but only 26.04 is wired for now.
DOCKER   ?= docker
REGISTRY ?= ghcr.io/lczyk/pats
VERSION  := $(shell cat VERSION)

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

# ----- egress proxy image -----
# build the egress proxy from this checkout, tagged both :latest and the version
# pin pats resolves to (:v<version>), so a pats installed from this clone finds
# it locally and skips the ghcr pull. needed when the checkout's version has no
# published image yet; a released install just pulls it from ghcr on first use.
.PHONY: egress-image
egress-image:  ## Build the egress proxy image locally (tags :latest and :v<version>)
	$(DOCKER) build \
		--tag "$(REGISTRY)/egress-proxy:latest" \
		--tag "$(REGISTRY)/egress-proxy:v$(VERSION)" \
		--file images/Dockerfile.egress-proxy \
		.
