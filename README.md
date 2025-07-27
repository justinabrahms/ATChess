# ATChess

A decentralized chess platform built on the AT Protocol. Play chess games that are stored in your personal data repository and federated across the network.

## Features

- **Decentralized Game Storage**: Games stored in players' AT Protocol repositories
- **Real Chess Validation**: Server-side move validation using professional chess engine
- **Interactive Web Interface**: Visual chessboard with drag-and-drop moves
- **Federation Ready**: Games work across different AT Protocol servers
- **Open Source**: Fully open implementation of chess on AT Protocol

## Architecture

ATChess consists of two main services:

### Protocol Service (`atchess-protocol`)

Handles AT Protocol interactions and chess game logic:
- Move validation using `notnil/chess` library
- Game state management with FEN/PGN notation
- AT Protocol record creation and storage
- REST API for game operations

### Web Service (`atchess-web`)

Serves the interactive chess interface:
- Visual chessboard with piece movement
- Real-time game state updates
- Game creation and move submission
- Static file serving for web assets

## Quick Start

### Prerequisites

- Docker and Docker Compose
- Go 1.21 or higher
- Make

### 1. Clone and Build

```bash
git clone <repository-url>
cd atchess
make build
```

### 2. Start Local Development Environment

**Option A: One-command setup**
```bash
./scripts/quick-start.sh
```

**If you encounter Docker issues:**
```bash
./scripts/troubleshoot-docker.sh
```

**Option B: Manual setup**
```bash
# Start local AT Protocol server
docker-compose up -d

# Create test accounts
./scripts/create-test-accounts.sh

# Start ATChess services
make run-protocol &  # Runs on port 8080
make run-web        # Runs on port 8081
```

### 3. Play Chess

1. Open http://localhost:8081 in your browser
2. Log in with your Bluesky account:
   - Enter your Bluesky handle (e.g., yourname.bsky.social)
   - Click "Login with Bluesky"
   - Authorize ATChess to access your account
3. Create games by challenging other Bluesky users
4. Accept challenges from the inbox
5. Play chess with real-time move synchronization

**OAuth Authentication:** ATChess uses AT Protocol OAuth for secure authentication. Users authorize ATChess to act on their behalf without sharing passwords. For production deployments, see [OAuth Setup Guide](docs/oauth-setup.md).

**Password Authentication (Legacy):** If OAuth is not configured, ATChess falls back to app password authentication. Users can create app passwords at https://bsky.app/settings/app-passwords

**Local Development:** For testing with a local PDS, see the [Local PDS Setup Guide](docs/local-pds-setup.md).

## AT Protocol Integration

ATChess uses custom lexicons for storing chess data:

### `app.atchess.game` - Game Records
- Player DIDs (white/black)
- Game status and result
- Current FEN position
- PGN notation
- Time control settings

### `app.atchess.move` - Move Records  
- Reference to parent game
- Move notation (SAN/UCI)
- FEN position after move
- Check/checkmate flags
- Move timestamps

### `app.atchess.challenge` - Game Invitations
- Challenger and challenged DIDs
- Color preferences
- Time control proposals
- Challenge status and expiration

## Configuration

### Protocol Service

The protocol service requires AT Protocol credentials to function. Configure via environment variables or `config.yaml`:

```bash
# Environment variables (recommended for production)
export ATPROTO_PDS_URL="https://bsky.social"
export ATPROTO_HANDLE="your-bot.bsky.social"
export ATPROTO_PASSWORD="xxxx-xxxx-xxxx-xxxx"  # App password from Bluesky
export ATPROTO_USE_DPOP="true"                 # Enable DPoP authentication
```

Or create `config.yaml`:
```yaml
server:
  host: 0.0.0.0
  port: 8080

atproto:
  pds_url: "https://bsky.social"
  handle: "your-bot.bsky.social"
  password: "xxxx-xxxx-xxxx-xxxx"
  use_dpop: true
```

### Web Service

The web service serves the user interface and doesn't require AT Protocol credentials. Users log in with their own Bluesky accounts through the web interface.

## Development

### Available Commands

```bash
# Building
make build          # Build both services
make protocol       # Build protocol service only
make web           # Build web service only

# Running
make run-protocol   # Start protocol service
make run-web       # Start web interface
make dev           # Start both services

# Testing
make test          # Run all tests
make test-integration # Run integration tests
make clean         # Remove build artifacts
```

### Project Structure

```
atchess/
├── cmd/                    # Application entry points
│   ├── protocol/          # AT Protocol service
│   └── web/               # Web interface service
├── internal/              # Internal packages
│   ├── atproto/           # AT Protocol client
│   ├── chess/             # Chess engine and logic
│   ├── config/            # Configuration management
│   └── web/               # Web handlers
├── lexicons/              # AT Protocol lexicon definitions
├── web/static/            # Static web assets
├── docs/                  # Documentation
├── test/                  # Test files
└── scripts/               # Development scripts
```

### Testing

```bash
# Unit tests
go test ./internal/chess/...

# Integration tests  
make test-integration

# Manual testing with two players
# See docs/testing-guide.md for detailed instructions

# Cross-PDS federation testing (advanced)
./scripts/test-dual-pds-setup.sh
```

## API Endpoints

### Protocol Service (localhost:8080)

- `GET /api/health` - Service health check
- `POST /api/games` - Create a new game
- `POST /api/games/{id}/moves` - Submit a move
- `POST /api/challenges` - Create a game challenge

### Example Usage

```bash
# Create a game
curl -X POST http://localhost:8080/api/games \
  -H "Content-Type: application/json" \
  -d '{"opponent_did": "did:plc:...", "color": "white"}'

# Make a move
curl -X POST http://localhost:8080/api/games/GAME_ID/moves \
  -H "Content-Type: application/json" \
  -d '{"from": "e2", "to": "e4", "fen": "..."}'
```

## Documentation

- **[Local PDS Setup](docs/local-pds-setup.md)** - Setting up AT Protocol server for development
- **[Testing Guide](docs/testing-guide.md)** - Comprehensive testing instructions
- **[CLAUDE.md](CLAUDE.md)** - Development guidelines and architecture notes

## Dependencies

- **Chess Logic**: `github.com/notnil/chess` - Professional chess move validation
- **Web Framework**: `github.com/gorilla/mux` - HTTP routing
- **Configuration**: `github.com/spf13/viper` - Configuration management  
- **Logging**: `github.com/rs/zerolog` - Structured logging
- **AT Protocol**: Direct HTTP implementation, no external dependencies

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes following the patterns in `CLAUDE.md`
4. Add tests for new functionality
5. Ensure all tests pass (`make test`)
6. Commit your changes (`git commit -m 'Add amazing feature'`)
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

### Development Guidelines

- Follow Go best practices and conventions
- Add tests for all new functionality
- Use the existing chess engine patterns for game logic
- Update documentation for new features
- Ensure AT Protocol compliance for new lexicons

## License

[Add your license here]

## Acknowledgments

- Built on the [AT Protocol](https://atproto.com/)
- Chess engine powered by [notnil/chess](https://github.com/notnil/chess)
- Inspired by the decentralized web movement

---

For detailed development instructions, see [CLAUDE.md](CLAUDE.md).
For testing with multiple players, see [docs/testing-guide.md](docs/testing-guide.md).
