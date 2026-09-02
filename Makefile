BINARY_NAME=metron
INSTALL_PATH=/usr/local/bin
COVERPROFILE=coverage.out
DOCKER_IMAGE=metron-test

.PHONY: build install test race cover vet fmt check \
        docker-build docker-test docker-race docker-cover test-live clean

# ---------------------------------------------------------------- build ----

build:
	go build -ldflags="-s -w" -o bin/$(BINARY_NAME) main.go

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

check: vet test race cover

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
	go test -tags=live -run Live -v -timeout 30m .

clean:
	rm -rf bin/ .tags $(COVERPROFILE)
