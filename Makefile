.PHONY: build protocol web run-protocol run-web dev-protocol dev-web dev test test-protocol test-web test-integration test-e2e test-local test-local-up test-local-down test-federation-up test-federation-up-ci test-federation-down test-federation-hosts-clean lint fmt clean

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
# Both modes below (atchess-1c9.24) additionally: (1) ensure the host
# resolves alice.pds.test/bob.pds.test (scripts/ensure-dual-pds-hosts.sh),
# and (2) extract the TLS proxy's local CA once it is up
# (scripts/extract-dual-pds-ca.sh) before creating accounts, since
# create-dual-pds-accounts.sh now talks to both PDSes over real,
# CA-verified HTTPS at those hostnames instead of plain
# http://localhost:<port>. See docker-compose.dual-pds.yml's header comment
# for the full topology.
#
# test-federation-up: LOCAL mode. Uses the real, public https://plc.directory
#   (see docker-compose.dual-pds.yml for why). Convenient for ad-hoc dev runs;
#   do not loop this repeatedly, it permanently publishes DID documents.
#
# test-federation-up-ci: CI mode. Brings up a hermetic local-plc service
#   first and waits for it to report healthy, then starts the PDSes pointed
#   at it instead of the public directory. Same harness/account script as
#   local mode -- only PDS_DID_PLC_URL differs.
#
# Both modes below also call scripts/check-dual-pds-mode.sh (atchess-1c9.22)
# before bringing anything up, and again to stamp a marker right after:
# switching modes with volumes intact would otherwise let
# create-dual-pds-accounts.sh's idempotent re-use path silently carry one
# mode's DIDs into the other mode's account file. See that script's header
# comment for the full rationale, including why the marker lives inside the
# pds-alice-data volume rather than in the repo or a container label.
test-federation-up:
	./scripts/ensure-dual-pds-hosts.sh
	mkdir -p certs/dual-pds
	./scripts/check-dual-pds-mode.sh local check
	docker compose -f docker-compose.dual-pds.yml up -d
	# Local mode never wants the CI-only hermetic did:plc service running
	# (atchess-1c9.22): if volumes/containers from a prior CI-mode run are
	# still present, stop them rather than leaving them running alongside
	# local mode's real-plc.directory PDSes. No-op (exit 0) if they were
	# never started.
	docker compose -f docker-compose.dual-pds.yml stop local-plc local-plc-db
	./scripts/check-dual-pds-mode.sh local stamp
	./scripts/extract-dual-pds-ca.sh
	./scripts/create-dual-pds-accounts.sh

test-federation-up-ci:
	./scripts/ensure-dual-pds-hosts.sh
	mkdir -p certs/dual-pds
	./scripts/check-dual-pds-mode.sh ci check
	docker compose -f docker-compose.dual-pds.yml --profile ci up -d --wait local-plc
	ATCHESS_PLC_URL=http://local-plc:2582 docker compose -f docker-compose.dual-pds.yml --profile ci up -d
	./scripts/check-dual-pds-mode.sh ci stamp
	./scripts/extract-dual-pds-ca.sh
	./scripts/create-dual-pds-accounts.sh

test-federation-down:
	docker compose -f docker-compose.dual-pds.yml --profile ci down -v

# Explicit, manual cleanup of the /etc/hosts entries scripts/ensure-dual-pds-hosts.sh
# adds. NOT called by test-federation-down -- see scripts/remove-dual-pds-hosts.sh's
# header comment for why automatic removal on every down would be actively harmful
# (repeated /etc/hosts rewrites, and multiple harnesses fighting over shared host state).
test-federation-hosts-clean:
	./scripts/remove-dual-pds-hosts.sh

# Code quality
lint:
	golangci-lint run

fmt:
	go fmt ./...

# Cleanup
clean:
	rm -rf bin/ tmp/