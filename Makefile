## ──────────────────────────────────────────────────────────────────────────────
## OpenClaw-Go  •  Makefile
## ──────────────────────────────────────────────────────────────────────────────

BINARY     := openclaw
VERSION    := $(shell cat VERSION 2>/dev/null || echo dev)
MODULE     := openclaw-go
IMAGE      := openclaw-go
BUILD_DIR  := dist

# Inject the version into the gateway package at link time.
LDFLAGS    := -s -w -X $(MODULE)/internal/gateway.Version=$(VERSION)

.PHONY: all build fmt vet test test-race e2e e2e-ps lint clean \
        run-gateway docker-build docker-run compose-up compose-smoke compose-test compose-test-integration \
        e2e-playwright smoke-rpc-ui check-openai-key release help smoke

## Default target
all: fmt vet test build

## ── Local build ───────────────────────────────────────────────────────────────

build: ## Build the binary into ./dist/
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build \
		-trimpath \
		-ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/$(BINARY) \
		./cmd/openclaw
	@echo "Built $(BUILD_DIR)/$(BINARY)  (version=$(VERSION))"

fmt: ## Run gofmt on all packages
	gofmt -w ./cmd ./internal

vet: ## Run go vet
	go vet ./...

test: ## Run all tests with -race and 30 s timeout
	go test -count=1 -timeout 30s -race ./...

test-race: test

e2e: build ## Run Go in-process E2E tests (no external server needed)
	go test ./e2e/... -v -count=1 -timeout 90s

e2e-sh: build ## Run shell E2E smoke test (Linux/macOS)
	bash scripts/e2e.sh

e2e-ps: build ## Run PowerShell E2E smoke test (Windows)
	pwsh -File scripts/e2e.ps1 -SkipBuild

smoke: ## Quick curl smoke (gateway must already be running; see scripts/smoke.sh)
	bash scripts/smoke.sh

check-openai-key: ## Verify OpenAI key via GET /v1/models (env or openclaw.json)
	go run ./scripts/check-openai-key.go

lint: ## Run staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

clean: ## Remove build artefacts
	rm -rf $(BUILD_DIR)

## ── Local run ─────────────────────────────────────────────────────────────────

run-gateway: ## Run the gateway locally (live reload via -race)
	go run -race ./cmd/openclaw gateway

## ── Docker ────────────────────────────────────────────────────────────────────

docker-build: ## Build the Docker image
	docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		-t $(IMAGE):latest \
		.

docker-run: ## Run the gateway container (bind-mount ~/.openclaw-go → /data; listens on 0.0.0.0)
	docker run --rm \
		-p 18789:18789 \
		-v "$$HOME/.openclaw-go:/data" \
		-e OPENCLAW_DATA_DIR=/data \
		-e OPENCLAW_CONFIG_PATH=/data/openclaw.json \
		-e OPENCLAW_GATEWAY_HOST=0.0.0.0 \
		-e OPENCLAW_GATEWAY_AUTH_TOKEN \
		-e OPENAI_API_KEY \
		-e ANTHROPIC_API_KEY \
		$(IMAGE):latest

compose-up: docker-build ## docker compose: run gateway (foreground)
	docker compose up gateway

compose-smoke: docker-build ## docker compose: gateway + smoke profile (exits after smoke)
	docker compose --profile smoke up --abort-on-container-exit --exit-code-from smoke

compose-test: ## docker compose: go test ./... in golang container (mounts repo)
	docker compose --profile test run --rm test

compose-test-integration: ## same as compose-test with -tags=integration (network)
	docker compose --profile test run --rm -e OPENCLAW_INTEGRATION_TESTS=1 test

e2e-playwright: ## Browser E2E: install e2e-ui deps + Playwright Chromium, run Playwright
	cd e2e-ui && npm install && npx playwright install chromium && npm test

## ── Cross-compile release artefacts ─────────────────────────────────────────

release: ## Build release binaries for common platforms
	@mkdir -p $(BUILD_DIR)/release
	@for PAIR in \
		linux/amd64 \
		linux/arm64 \
		darwin/amd64 \
		darwin/arm64 \
		windows/amd64; do \
		OS=$$(echo $$PAIR | cut -d/ -f1); \
		ARCH=$$(echo $$PAIR | cut -d/ -f2); \
		OUT=$(BUILD_DIR)/release/$(BINARY)-$(VERSION)-$$OS-$$ARCH; \
		[ "$$OS" = "windows" ] && OUT=$$OUT.exe; \
		echo "  building $$OS/$$ARCH → $$OUT"; \
		CGO_ENABLED=0 GOOS=$$OS GOARCH=$$ARCH \
			go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $$OUT ./cmd/openclaw; \
	done
	@echo "Release artefacts written to $(BUILD_DIR)/release/"
	@ls -lh $(BUILD_DIR)/release/

## ── Help ─────────────────────────────────────────────────────────────────────

help: ## Print this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
