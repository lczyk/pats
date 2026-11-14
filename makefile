.SUFFIXES:

GO_DIRS  := ./cmd ./internal ./src ./images
GO_PATHS := $(GO_DIRS) ./version.go
SRCS     := $(shell find $(GO_DIRS) -name '*.go') ./version.go
# go:embed inputs: the version string, what images/embed.go bakes in, and
# the default clis and harnesses
EMBEDS := ./VERSION ./images/provision-sandbox.sh ./internal/machine/defaults.yaml

.PHONY: help
help:  ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_.\/-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ build

.PHONY: build
build: ./bin/pats  ## Build the pats binary into ./bin (upx-compressed if available)

./bin/pats: $(SRCS) $(EMBEDS) makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/pats ./cmd/pats
	@if command -v upx >/dev/null 2>&1; then \
		upx ./bin/pats || echo "upx failed, skipping compression"; \
	fi

.PHONY: install
install: ./bin/pats  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(PWD)/bin/pats" "$(HOME)/.local/bin/pats"

.PHONY: clean
clean:  ## Remove the binary and the image stamps (forces an image rebuild next run)
	rm -f ./bin/pats
	rm -rf .stamp

##@ checks

.PHONY: verify
verify: lint test  ## Aggregate gate: lint + test

.PHONY: lint
lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l $(GO_PATHS)); \
	if [ -n "$$out" ]; then \
		echo "unformatted files:"; echo "$$out"; exit 1; \
	fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w $(GO_PATHS)

.PHONY: test
test:  ## Run the test suite with the race detector
	go test -race ./...

.PHONY: cover
cover:  ## Show test coverage per function in the CLI
	@out=$$(mktemp); trap 'rm -f "$$out"' EXIT; \
	go test -coverprofile="$$out" ./... && go tool cover -func="$$out"

FUZZTIME ?= 10s

# go test takes one fuzz target per run, so each Fuzz* func in the tree gets its own.
.PHONY: fuzz
fuzz:  ## Run all fuzz targets (narrow via FUZZTIME=..)
	@for t in $$(grep -roE --include='*_test.go' '^func Fuzz[A-Za-z0-9_]+' $(GO_DIRS) | sed -E 's|/[^/]*:func |:|' | sort); do \
		echo "go test $${t%%:*} -fuzz $${t##*:}"; \
		go test "$${t%%:*}" -run - -fuzz "^$${t##*:}\$$" -fuzztime $(FUZZTIME) || failed="$$failed $${t##*:}"; \
	done; \
	if [ -n "$$failed" ]; then echo "failed:$$failed"; exit 1; fi

.PHONY: bench
bench:  ## Run all benchmarks
	go test ./... -run - -bench . -benchmem

# every model in docs/spec/fizz must pass as written. publish.fizz also keeps the designs it rejects:
# snapshot and selfres must still fail R2 (the race the aliases job exists for).
.PHONY: fizz
fizz:  ## Model-check the fizzbee specs in docs/spec/fizz
	@if ! command -v fizz >/dev/null 2>&1; then echo "fizz not found, skipping (https://github.com/fizzbee-io/fizzbee)"; exit 0; fi; \
	cd docs/spec/fizz && for f in *.fizz; do \
		printf '%-24s ' "$$f"; \
		timeout -k 10 300 fizz $$f 2>&1 | grep -E '^PASSED' || { echo "not passed"; exit 1; }; \
	done; \
	for d in snapshot selfres; do \
		printf '%-24s ' "publish.fizz $$d"; \
		timeout -k 10 300 fizz --preinit-hook "DESIGN = \"$$d\"" publish.fizz 2>&1 | grep -E '^FAILED' || { echo "passed, expected to fail"; exit 1; }; \
	done

# needs a logged-in gh, or GH_TOKEN. ci verifies with this same version.
.PHONY: actions-lock
actions-lock:  ## Re-pin the workflows' actions in .github/workflows/actions.lock (flags via ARGS=.., e.g. --verify, --relock)
	go run github.com/github/gh-actions-lock/cmd/gh-actions-lock@v0.4.1 $(ARGS)

##@ images

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
images: $(SANDBOX_STAMPS)  ## Build sandbox images locally, tagged :v<version>, :latest and :<ubuntu> (narrow via VER=.. ARCH=..)

.stamp/sandbox-%: FORCE | .stamp
	@hack/build_image.sh sandbox-$*

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
