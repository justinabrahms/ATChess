.PHONY: check-supervised deploy verify test-verbose build protocol web run-protocol run-web dev-protocol dev-web dev test test-protocol test-web test-integration test-e2e test-local test-local-up test-local-down test-federation-up test-federation-up-ci test-federation-down test-federation-hosts-clean lint fmt clean

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
#
# THE FLEET CONTRACT (pipeline#adr-011, #adr-007). `make test` is the gate CI
# runs on every pull request and the thing that must be green before `make
# deploy` is even attempted. It runs two suites:
#
#   go test ./...   the Go suite, including the oracles in docs/ORACLES.md
#   test/*.sh       shipped demos, which are permanent regression tests
#
# test/pending/ is EXCLUDED on purpose. A demo there belongs to a slice that
# has not shipped yet -- it is written before the code (pipeline#adr-003) and
# is expected to be red until the slice lands. `pipe deploy` promotes it out of
# pending/ once it passes, at which point this target starts running it and it
# never goes red again without someone noticing.
#
# test/e2e and test/integration need a live PDS stack and are not part of this
# gate; they have their own targets below.
test:
	go test ./internal/... ./cmd/...
	@found=0; failed=0; \
	 for d in test/*.sh; do \
	   [ -e "$$d" ] || continue; \
	   found=$$((found+1)); \
	   echo "--- $$d"; \
	   bash "$$d" || failed=$$((failed+1)); \
	 done; \
	 echo "demos: $$found run, $$failed failed"; \
	 [ "$$failed" -eq 0 ]

test-verbose:
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
# as their very first step -- before ensure-dual-pds-hosts.sh, before
# `mkdir -p certs/dual-pds`, before bringing anything up -- and again to
# stamp a marker on both PDS volumes right after (atchess-1c9.79): switching
# modes with volumes intact would otherwise let create-dual-pds-accounts.
# sh's idempotent re-use path silently carry one mode's DIDs into the other
# mode's account file. Running the check first means a refusal touches
# nothing at all. See that script's header comment for the full rationale,
# including why the markers live inside the pds-alice-data/pds-bob-data
# volumes rather than in the repo or a container label.
# BLAST-RADIUS GATE (check-public-plc-blast-radius.sh) runs FIRST here --
# ahead of the mode check, any mkdir, and anything being brought up. Local
# mode publishes permanent, undeletable DID documents to the public
# https://plc.directory, so an unattended caller has to be refused while a
# refusal still touches nothing at all. The prose warning in
# docker-compose.dual-pds.yml's header protects a human who reads it once;
# it is no protection against a runner that retries. test-federation-up-ci
# is hermetic and is deliberately NOT gated.
test-federation-up:
	./scripts/check-public-plc-blast-radius.sh
	./scripts/check-dual-pds-mode.sh local check
	./scripts/ensure-dual-pds-hosts.sh
	mkdir -p certs/dual-pds
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
	./scripts/check-dual-pds-mode.sh ci check
	./scripts/ensure-dual-pds-hosts.sh
	mkdir -p certs/dual-pds
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

# Supervised-path gate (decision atchess-b2d.2). web/static/ has no oracle --
# no rendering test, no DOM assertion, no interaction test -- so a change there
# that renders a blank board passes the whole suite. Agent work on those paths
# is allowed; agent work that MERGES ITSELF is not. Deliberately not folded
# into `make test`: a human at a terminal always passes this gate, so wiring it
# into the default suite would only ever fire in CI, where it cannot yet tell a
# person's frontend PR from a robot's. See docs/ORACLES.md.
check-supervised:
	./scripts/check-supervised-paths.sh

# DEPLOY -- the fleet's release path (pipeline#adr-006, #adr-015, #adr-018).
#
# CI runs this, and only on a green master. Deploy is a CONSEQUENCE of green,
# never a command you type: running it by hand against the live services is how
# you ship code no test agreed to. A red suite skips it.
#
# The repo declares its own deploy; there is no fleet config format. This
# mirrors what the GitHub Actions workflow already does -- build static
# binaries, drop them in place, restart the units -- so that the gitea runner
# and GitHub cannot disagree about what deployed means.
DEPLOY_DIR ?= /srv/atchess/app/bin
PROTOCOL_UNIT ?= atchess-protocol
WEB_UNIT ?= atchess-web

.PHONY: deploy verify
deploy: build
	@install -d "$(DEPLOY_DIR)"
	@install -m 0755 bin/atchess-protocol "$(DEPLOY_DIR)/atchess-protocol"
	@install -m 0755 bin/atchess-web "$(DEPLOY_DIR)/atchess-web"
	@systemctl --user restart "$(PROTOCOL_UNIT)" "$(WEB_UNIT)" 2>/dev/null \
	  || sudo systemctl restart "$(PROTOCOL_UNIT)" "$(WEB_UNIT)"
	@echo "deployed to $(DEPLOY_DIR); restarted $(PROTOCOL_UNIT) $(WEB_UNIT)"
	@$(MAKE) --no-print-directory verify

# A 200 IS NOT PROOF. The flags service taught this rig that a process can
# restart, come back healthy and come back EMPTY -- which passes a liveness
# check while every feature reads off. So verify asserts something only a
# working build can produce: the protocol service validates a real chess
# position, which exercises the engine rather than the listener.
PROTOCOL_URL ?= http://localhost:8080
WEB_URL ?= http://localhost:8081

verify:
	@set -e; \
	 curl -fsS --max-time 5 "$(PROTOCOL_URL)/health" >/dev/null \
	   || { echo "verify: protocol service is not answering at $(PROTOCOL_URL)"; exit 1; }; \
	 curl -fsS --max-time 5 "$(WEB_URL)/" >/dev/null \
	   || { echo "verify: web service is not answering at $(WEB_URL)"; exit 1; }; \
	 echo "verify: both services answering"

# Code quality
lint:
	golangci-lint run

fmt:
	go fmt ./...

# Cleanup
clean:
	rm -rf bin/ tmp/