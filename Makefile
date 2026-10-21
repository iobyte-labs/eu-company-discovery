# Development tasks. Everything here runs offline.
#
# VERSION is stamped into the binary from the git tag so a released build can say what it
# is. A dirty or untagged tree says so rather than claiming a version it is not.

VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/iobyte-labs/eu-company-discovery.Version=$(VERSION)

GO      ?= go
BIN     := bin/eucd

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the eucd binary for this machine
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/eucd

.PHONY: install
install: ## Install eucd into GOBIN
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/eucd

.PHONY: test
test: ## Run every test
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run tests and report coverage per package
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -25

.PHONY: fmt
fmt: ## Format the tree
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint if it is installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; running go vet instead"; \
		$(GO) vet ./...; \
	fi

.PHONY: tidy
tidy: ## Tidy and verify go.mod / go.sum
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: check
check: fmt vet test ## Format, vet and test — run this before committing

# A statically linked binary. CGO is off because modernc.org/sqlite is pure Go, which is
# why this cross-compiles and runs in a scratch image at all.
.PHONY: build-static
build-static: ## Build a static linux/amd64 binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/eucd-linux-amd64 ./cmd/eucd

.PHONY: build-all
build-all: ## Cross-compile for linux and darwin, amd64 and arm64
	@for os in linux darwin; do \
		for arch in amd64 arm64; do \
			echo "building $$os/$$arch"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				$(GO) build -trimpath -ldflags '$(LDFLAGS)' \
				-o bin/eucd-$$os-$$arch ./cmd/eucd || exit 1; \
		done; \
	done

.PHONY: docker
docker: ## Build the container image
	docker build -t eucd:$(VERSION) .

.PHONY: validate
validate: build ## Validate the committed configuration
	$(BIN) config validate

.PHONY: demo
demo: build ## Run a throwaway discovery against the committed seed config
	@rm -f /tmp/eucd-demo.db
	$(BIN) --db /tmp/eucd-demo.db discover
	$(BIN) --db /tmp/eucd-demo.db discover
	$(BIN) --db /tmp/eucd-demo.db companies list
	@rm -f /tmp/eucd-demo.db

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out
