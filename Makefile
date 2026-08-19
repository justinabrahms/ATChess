.PHONY: build protocol web run-protocol run-web dev-protocol dev-web dev test test-protocol test-web test-integration test-e2e test-local test-local-up test-local-down test-federation-up test-federation-up-ci test-federation-down lint fmt clean

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

test-local:
	./scripts/test-local.sh

test-local-up:
	./scripts/test-local.sh --up

test-local-down:
	./scripts/test-local.sh --down

# Dual-PDS federated test harness (two independent PDS instances + two accounts)
#
# test-federation-up: LOCAL mode. Uses the real, public https://plc.directory
#   (see docker-compose.dual-pds.yml for why). Convenient for ad-hoc dev runs;
#   do not loop this repeatedly, it permanently publishes DID documents.
#
# test-federation-up-ci: CI mode. Brings up a hermetic local-plc service
#   first and waits for it to report healthy, then starts the PDSes pointed
#   at it instead of the public directory. Same harness/account script as
#   local mode -- only PDS_DID_PLC_URL differs.
test-federation-up:
	docker compose -f docker-compose.dual-pds.yml up -d
	./scripts/create-dual-pds-accounts.sh

test-federation-up-ci:
	docker compose -f docker-compose.dual-pds.yml --profile ci up -d --wait local-plc
	ATCHESS_PLC_URL=http://local-plc:2582 docker compose -f docker-compose.dual-pds.yml --profile ci up -d
	./scripts/create-dual-pds-accounts.sh

test-federation-down:
	docker compose -f docker-compose.dual-pds.yml --profile ci down -v

# Code quality
lint:
	golangci-lint run

fmt:
	go fmt ./...

# Cleanup
clean:
	rm -rf bin/ tmp/