.PHONY: build protocol web run-protocol run-web dev-protocol dev-web dev test test-protocol test-web test-integration test-e2e lint fmt clean

# Build commands
build: protocol web

protocol:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o bin/atchess-protocol cmd/protocol/main.go

web:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o bin/atchess-web cmd/web/main.go

# Local development builds (for macOS)
protocol-local:
	go build -o bin/atchess-protocol-local cmd/protocol/main.go

web-local:
	go build -o bin/atchess-web-local cmd/web/main.go

# Development
run-protocol: protocol-local
	./bin/atchess-protocol-local

run-web: web-local
	./bin/atchess-web-local

# Development with auto-reload
dev-protocol:
	@echo "Starting protocol service with auto-reload..."
	@command -v air >/dev/null 2>&1 || { echo "Installing air for auto-reload..."; go install github.com/air-verse/air@latest; }
	@air -c .air-protocol.toml

dev-web:
	@echo "Starting web service with auto-reload..."
	@command -v air >/dev/null 2>&1 || { echo "Installing air for auto-reload..."; go install github.com/air-verse/air@latest; }
	@air -c .air-web.toml

dev:
	@echo "Starting both services in development mode with auto-reload..."
	@make dev-protocol &
	@make dev-web &
	@wait

# Testing
test:
	go test -v ./...

test-protocol:
	go test -v ./internal/atproto/... ./internal/chess/... ./internal/config/...

test-web:
	go test -v ./internal/web/...

test-integration:
	go test -v -tags=integration ./test/integration/...

test-e2e:
	./scripts/run-e2e-tests.sh

# Code quality
lint:
	golangci-lint run

fmt:
	go fmt ./...

# Cleanup
clean:
	rm -rf bin/ tmp/