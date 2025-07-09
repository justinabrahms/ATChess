# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ATChess is a decentralized chess platform built on the AT Protocol. It consists of two main components:
- **Protocol Service**: Handles AT Protocol interactions, game state management, and federation
- **Web Application**: Interactive chess UI for playing and viewing games

## Development Commands

The following commands are available for development:

```bash
# Build commands
make build          # Build both protocol and web binaries
make protocol       # Build only the protocol service
make web           # Build only the web application

# Development
make run-protocol   # Run the protocol service locally (port 8080)
make run-web       # Run the web server locally (port 8081)
make dev           # Run both services in development mode

# Testing
make test          # Run all tests
make test-protocol # Test protocol service and chess logic
make test-web      # Test web application
make test-integration # Run integration tests

# Code quality
make lint          # Run golangci-lint
make fmt           # Format code with gofmt
make clean         # Remove build artifacts
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