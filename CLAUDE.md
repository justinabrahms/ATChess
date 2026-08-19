# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ATChess is a decentralized chess platform built on the AT Protocol. It consists of two main components:
- **Protocol Service**: Handles AT Protocol interactions, game state management, and federation
- **Web Application**: Interactive chess UI for playing and viewing games

## Development Commands

```bash
# Build commands
make build          # Build both protocol and web binaries
make protocol       # Build only the protocol service
make web           # Build only the web application

# Development (production builds)
make run-protocol   # Run the protocol service locally (port 8080)
make run-web       # Run the web server locally (port 8081)

# Development with auto-reload
make dev-protocol   # Run protocol service with auto-reload on file changes
make dev-web       # Run web server with auto-reload on file changes
make dev           # Run both services with auto-reload

# Testing
make test          # Run all tests
make test-protocol # Test protocol service and chess logic
make test-web      # Test web application
make test-e2e      # Run end-to-end tests

# Code quality
make lint          # Run golangci-lint
make fmt           # Format code with gofmt
make clean         # Clean build artifacts and temp files
```

## Architecture

### AT Protocol Integration

The protocol service implements custom lexicons for chess data:
- `app.atchess.game` - Game metadata and state
- `app.atchess.move` - Individual moves
- `app.atchess.challenge` - Game invitations

Key architectural decisions:
- Games are stored in players' personal data repositories
- Moves are validated server-side using the notnil/chess library
- FEN notation tracks board state
- PGN notation preserves game history
- Direct HTTP calls to AT Protocol for simplicity and reliability

### Code Organization

```
cmd/
├── protocol/    # Entry point for AT Protocol service (port 8080)
└── web/         # Entry point for web server (port 8081)

internal/
├── atproto/     # AT Protocol client and interactions
├── chess/       # Chess engine using notnil/chess library
├── config/      # Configuration management with Viper
└── web/         # HTTP handlers and web UI logic

lexicons/        # AT Protocol lexicon definitions (JSON)
web/static/      # Static web assets (HTML, CSS, JS)
docs/            # Documentation including PDS setup and testing guides
test/            # Test files including integration tests
scripts/         # Development and setup scripts
```

### Development Workflow

1. **Local PDS Setup**: Required for testing AT Protocol integration
   - Use Docker Compose with the official PDS image
   - Run `docker-compose up -d` to start the PDS on port 3000
   - Create test accounts with `./scripts/create-test-accounts.sh`
   - See `docs/local-pds-setup.md` for detailed instructions

2. **Testing Strategy**:
   - Unit tests for chess logic using `internal/chess/engine_test.go`
   - Integration tests with real chess games in `test/integration/`
   - Manual testing with two accounts via web interface
   - API testing using curl commands in testing guide

3. **Key Implementation Notes**:
   - Chess moves validated using notnil/chess library before AT Protocol storage
   - Games stored in both players' repositories for redundancy
   - Direct HTTP client for AT Protocol interactions (no external dependencies)
   - Comprehensive error handling for invalid moves and network failures
   - Interactive web UI with visual chessboard for easy testing

## Common Tasks

### Adding a New Lexicon
1. Define the lexicon JSON in `lexicons/` directory
2. Update `internal/atproto/client.go` to handle the new record type
3. Add API endpoints in `internal/web/service.go`
4. Add tests for the new functionality
5. Update documentation

### Implementing Chess Features
1. Add chess logic to `internal/chess/engine.go` using notnil/chess library
2. Update lexicons in `lexicons/` if new data structures needed
3. Add API endpoints in `internal/web/service.go`
4. Update frontend JavaScript in `web/static/index.html`
5. Add tests in `internal/chess/engine_test.go`

### Testing AT Protocol Integration
1. Start local PDS: `docker-compose up -d`
2. Create test accounts: `./scripts/create-test-accounts.sh`
3. Build and run services: `make build && make run-protocol & make run-web`
4. Run tests: `make test` and `make test-integration`
5. Manual testing: Follow `docs/testing-guide.md`

### Dependencies and Libraries
- **Chess Engine**: Uses `github.com/notnil/chess v1.9.0` for move validation
- **Web Framework**: Uses `github.com/gorilla/mux v1.8.1` for HTTP routing
- **Configuration**: Uses `github.com/spf13/viper v1.18.2` for config management
- **Logging**: Uses `github.com/rs/zerolog v1.31.0` for structured logging
- **AT Protocol**: Direct HTTP calls, no external AT Protocol library dependencies

## Deployment and Infrastructure

### Production Deployment
- **Server**: Uses Caddy web server (not nginx) for reverse proxy and SSL termination
- **Auto-deploy**: GitHub Actions automatically deploys on push to main
- **File Structure**: Binaries deployed to `/srv/atchess/app/bin/`
- **Services**: Managed by systemd (atchess-protocol and atchess-web)
- **Permissions**: Deploy user needs group membership, not sudo

### OAuth Configuration
- **Client Metadata**: Served dynamically at `/client-metadata.json`
- **Key Generation**: Run `go run github.com/justinabrahms/atchess/cmd/generate-oauth-keys@main` locally
- **Reverse Proxy**: Service respects `X-Forwarded-Proto` header for correct HTTPS URLs
- **Caddy Config**: Must include routes for `/client-metadata.json`, `/api/*`, and `/`

### Common Deployment Issues
1. **OAuth "missing code or state" error**: Usually means `/client-metadata.json` is not properly routed
2. **HTTP vs HTTPS mismatch**: Service must detect HTTPS via `X-Forwarded-Proto` header
3. **Binary permissions**: Deploy user needs write access via group membership
4. **Setup script**: Only handles configuration, not binary deployment (that's done by CI/CD)
<!-- BEGIN domestique (managed) -->
# Orchestration policy

This session is the **orchestrator**. Your job is planning, delegation, and review — not implementation.

## Roles
- **You (main session, planning model):** decompose work, hold the plan, delegate implementation and review, adjudicate the results, decide what's next. Write code yourself only for trivial one-line edits.
- **`implementer` subagent (Sonnet):** executes one bounded task at a time in its own context and reports back a summary.
- **`reviewer` subagent (Opus):** independently verifies a completed task in a fresh context — inspects the real diff, reads the changed files, runs the tests — and reports a pass/fail verdict against the bead's done-criteria. A stronger, non-peer check than the implementer. Does not fix anything; reviewing is its only job.

## Work tracking: beads
- The plan of record lives in beads (`bd`), not in markdown TODO lists.
- Decompose a goal into an epic + bounded tasks with dependencies using `/decompose`.
- Select the next unit of work with `bd ready` — it returns only unblocked, actionable tasks.
- Record durable insight with `bd remember "<insight>"`. Do not create MEMORY.md files.

## Writing briefs
Plans, bead descriptions, and delegation briefs are executed by a separate model with no access to your reasoning. When you write them:
- Write numbered steps; each step names an action, a target file/symbol, and an acceptance criterion.
- Spell out edge cases and error handling — do not leave them implicit.
- Flag ambiguities explicitly rather than resolving them silently.

## Delegation loop
1. `bd ready` → pick the highest-priority unblocked task.
2. Delegate it to the `implementer` subagent with a precise brief and the bead id.
3. When the implementer returns, delegate verification to the `reviewer` subagent with the same bead id and its done-criteria. The reviewer inspects the real diff, reads the changed files, and runs the tests in a fresh context — judging the work against the done-criteria, not against the implementer's summary — and returns a pass/fail verdict.
4. Adjudicate. Weigh the reviewer's verdict against the implementer's summary: if they agree the work is done, close the bead and commit its changes (one commit, bead id in the message); if the reviewer reports gaps, reopen the bead or file a follow-up and route the fix back to the implementer. Read the diff yourself only when the two reports conflict or the verdict is ambiguous — delegating the review is the point.
5. **Stop and report to the human before dispatching the next task.** Do not drain the queue unattended unless explicitly told to.

## Unattended epic mode (/goal)
- The default remains **stop-and-report between beads** (rule 5 of the Delegation loop above). Nothing changes that by itself.
- A `/goal <epic-id>` invocation is the **only** thing that authorizes continuous, unattended dispatch across an epic's beads. That authorization is scoped to the named epic, expires the instant the epic completes or any stop condition fires, and never carries over to another epic or a later session.
- Unattended runs happen on a **dedicated epic branch** and never commit to the default branch — the human reviews and merges that branch by hand; the loop never merges or pushes.
- The core invariants still hold even while unattended: **one bead in flight at a time, one commit per bead, and never close a bead the reviewer didn't pass.**
- For the full loop mechanics and the complete list of stop conditions, see `.claude/commands/goal.md` — they are not restated here.

## Discipline
- One task in flight at a time. Bounded WIP.
- Subagents return summaries, never full file dumps. Your context is the constraint — keep it lean, don't re-read large outputs.
- Do not spawn agent teams for this sequential pipeline. Subagents only.
- At session end ("land the plane"): file any loose discovered work as beads, then export and commit (`bd export`, then commit `.beads/`). `bd export` writes the git-tracked `.beads/*.jsonl` — that JSONL is the versioned snapshot. There is no `bd sync`; bd is Dolt-backed now, and `bd dolt commit` records local Dolt history only (`.beads/dolt/` is gitignored, so it never affects a clean tree).
<!-- END domestique -->


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->
