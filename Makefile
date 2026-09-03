BINARY_NAME=metron
INSTALL_PATH=/usr/local/bin
COVERPROFILE=coverage.out
DOCKER_IMAGE=metron-test

.PHONY: build install test race cover vet fmt lint check \
        docker-build docker-test docker-race docker-cover test-live \
        release-check release-snapshot clean

# ---------------------------------------------------------------- build ----

VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY_NAME) ./cmd/metron

install: build
	sudo cp bin/$(BINARY_NAME) $(INSTALL_PATH)/$(BINARY_NAME)

# ------------------------------------------------------- tests (host) ------
# Hermetic: no Ollama server, no ripgrep, no Universal Ctags required.

test:
	go test ./...

race:
	go test -race -count=1 ./...

cover:
	go test -covermode=count -coverprofile=$(COVERPROFILE) ./...
	go tool cover -func=$(COVERPROFILE) | tail -1
	@echo "HTML report: go tool cover -html=$(COVERPROFILE)"

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Needs golangci-lint on PATH; CI runs this too.
# GOLANGCI_VERSION must match the pin in .github/workflows/ci.yml: a newer
# release can add checks and fail a PR that touched none of the code they cover.
GOLANGCI_VERSION?=v2.13.2

lint:
	@have=$$(golangci-lint version 2>/dev/null | grep -o 'version [^ ]*' | cut -d' ' -f2); \
	want=$$(echo $(GOLANGCI_VERSION) | tr -d v); \
	if [ "$$have" != "$$want" ]; then \
	  echo "warning: golangci-lint $$have on PATH, CI pins $$want"; \
	  echo "         go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)"; \
	fi
	golangci-lint run

check: vet test race cover

# ---------------------------------------------------------- release -------
# Both need goreleaser on PATH. CI runs them on every PR, so a broken release
# config fails there rather than at tag time.

release-check:
	goreleaser check

release-snapshot:
	goreleaser build --snapshot --clean

# ----------------------------------------------------- tests (docker) ------
# Adds the `integration` tests, which need real ripgrep, Universal Ctags and
# git. Runs unprivileged so the permission-failure cases are not skipped.

docker-build:
	docker build -f Dockerfile.test -t $(DOCKER_IMAGE) .

docker-test: docker-build
	docker run --rm $(DOCKER_IMAGE) go test -tags=integration ./...

docker-race: docker-build
	docker run --rm $(DOCKER_IMAGE) go test -tags=integration -race -count=1 ./...

docker-cover: docker-build
	docker run --rm $(DOCKER_IMAGE) sh -c \
	  "go test -tags=integration -covermode=count -coverprofile=/tmp/c.out ./... && \
	   go tool cover -func=/tmp/c.out | tail -1"

# ------------------------------------------------------- tests (live) ------
# Talks to a real Ollama server with a real model. Set METRON_TEST_MODEL to
# pin a model; otherwise the smallest tool-capable installed model is used.

test-live:
	go test -tags=live -run Live -v -timeout 30m ./cmd/metron

clean:
	rm -rf bin/ dist/ .tags $(COVERPROFILE)
