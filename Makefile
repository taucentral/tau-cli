# tau-cli Makefile
#
# Canonical tau binary. All targets are phony unless noted.
# The build produces a statically-linked binary (CGO disabled by default).

BINARY   := bin/tau
PKG      := ./cmd/tau
GOFLAGS  := -trimpath
LDFLAGS  := -s -w
CGO_ENV  := CGO_ENABLED=0
TEST_TIMEOUT := 60s

.PHONY: all build test e2e lint install run tidy fmt vet check clean help

all: build

# build: produce a static binary at ./bin/tau
build:
	$(CGO_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

# test: run unit + integration tests (excludes e2e by default)
test:
	go test -timeout $(TEST_TIMEOUT) ./...

# e2e: run end-to-end tests (modes_integration_test.go)
e2e:
	TAU_RUN_E2E=1 go test -timeout 120s ./test/e2e/...

# lint: run golangci-lint with the project config
lint:
	golangci-lint run ./...

# install: install the binary to $GOBIN
install:
	$(CGO_ENV) go install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(PKG)

# run: build-and-run convenience target (forward ARGS as CLI args)
run: build
	./$(BINARY) $(ARGS)

# tidy: sync go.mod/go.sum
tidy:
	go mod tidy

# fmt + vet: standalone formatters
fmt:
	gofmt -s -w .
	goimports -w -local github.com/taucentral/tau-cli .

vet:
	go vet ./...

# check: fast pre-commit gate (fmt + vet + lint)
check: vet lint

# clean: remove build artifacts
clean:
	rm -rf bin coverage.out

help:
	@echo "tau-cli Makefile targets:"
	@echo "  build    - produce a static binary at ./bin/tau"
	@echo "  test     - run unit + integration tests"
	@echo "  e2e      - run end-to-end tests"
	@echo "  lint     - run golangci-lint"
	@echo "  install  - go install the binary"
	@echo "  run      - build + run with ARGS=..."
	@echo "  tidy     - go mod tidy"
	@echo "  fmt      - gofmt + goimports"
	@echo "  vet      - go vet"
	@echo "  check    - vet + lint"
	@echo "  clean    - remove build artifacts"
