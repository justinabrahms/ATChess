# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ATChess is a decentralized chess platform built on the AT Protocol. It consists of two main components:
- **Protocol Service**: Handles AT Protocol interactions, game state management, and federation
- **Web Application**: Interactive chess UI for playing and viewing games

## Development Commands

Since this is a new project, the following commands will be established:

```bash
# Build commands (to be implemented)
make build          # Build both protocol and web binaries
make protocol       # Build only the protocol service
make web           # Build only the web application

# Development
make run-protocol   # Run the protocol service locally
make run-web       # Run the web server locally
make dev           # Run both services in development mode

# Testing
make test          # Run all tests
make test-protocol # Test protocol service
make test-web      # Test web application

# Code quality
make lint          # Run golangci-lint
make fmt           # Format code with gofmt
```

## Architecture

### AT Protocol Integration

The protocol service implements custom lexicons for chess data:
- `app.chess.game` - Game metadata and state
- `app.chess.move` - Individual moves
- `app.chess.challenge` - Game invitations

Key architectural decisions:
- Games are stored in players' personal data repositories
- Moves are validated server-side before recording
- FEN notation tracks board state
- PGN notation preserves game history

### Code Organization

```
cmd/
├── protocol/    # Entry point for AT Protocol service
└── web/         # Entry point for web server

internal/
├── atproto/     # AT Protocol client and interactions
├── chess/       # Chess engine and game logic
├── config/      # Configuration management
├── lexicon/     # AT Protocol lexicon definitions
├── repository/  # Data access layer
└── web/         # HTTP handlers and web UI logic
```

### Development Workflow

1. **Local PDS Setup**: Required for testing AT Protocol integration
   - Use the AT Protocol PDS Docker image
   - Configure with test DIDs and handles
   - Update config.yaml with local PDS endpoint

2. **Testing Strategy**:
   - Unit tests for chess logic (internal/chess)
   - Integration tests for AT Protocol operations
   - E2E tests with multiple test accounts

3. **Key Implementation Notes**:
   - Always validate moves server-side using the chess engine
   - Store games in both players' repositories for redundancy
   - Use AT Protocol's built-in federation for cross-PDS play
   - Implement proper error handling for network failures

## Common Tasks

### Adding a New Lexicon
1. Define the lexicon in `internal/lexicon/`
2. Generate Go types from the lexicon
3. Implement handlers in `internal/atproto/`
4. Add tests for the new functionality

### Implementing Chess Features
1. Add logic to `internal/chess/` 
2. Update lexicons if new data structures needed
3. Add API endpoints in `internal/web/`
4. Update frontend to use new features

### Testing AT Protocol Integration
1. Start local PDS instance
2. Create test accounts
3. Run integration tests with `make test-integration`