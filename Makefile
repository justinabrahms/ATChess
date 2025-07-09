.PHONY: build protocol web run-protocol run-web dev test test-protocol test-web test-integration test-e2e lint fmt clean

# Build commands
build: protocol web

protocol:
	go build -o bin/atchess-protocol cmd/protocol/main.go

web:
	go build -o bin/atchess-web cmd/web/main.go

# Development
run-protocol: protocol
	./bin/atchess-protocol

run-web: web
	./bin/atchess-web

dev:
	@echo "Starting both services in development mode..."
	@make run-protocol &
	@make run-web &
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
	rm -rf bin/