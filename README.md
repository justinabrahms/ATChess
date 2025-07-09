# ATChess: Chess on AT Protocol

ATChess is a decentralized chess platform built on the AT Protocol. It enables players to engage in chess matches across the AT Protocol network while maintaining ownership of their game data.

## Features

- **Play chess games** with anyone in the AT Protocol network
- **Store game data** in your personal data repository
- **View active games** from your profile and network
- **Explore game history** with full PGN notation support
- **Analyze matches** with integrated board visualization
- **Challenge friends** through AT Protocol social graph
- **Follow games** from players in your network

## Architecture

ATChess consists of two main components:

### 1. Protocol Service (`atchess-protocol`)

This service handles all AT Protocol interactions:

- Custom lexicons for chess game data
- Game state management and validation
- Player matchmaking and challenges
- Game record creation and updates
- Federation with other AT Protocol instances

### 2. Web Application (`atchess-web`)

A web interface for playing and viewing games:

- Interactive chess board UI
- Game listing and filtering
- Move validation and submission
- Game history exploration
- User profile and stats
- Network activity feed

## Getting Started

### Prerequisites

- Go 1.21+
- AT Protocol Personal Data Server (PDS) access
- Valid AT Protocol account/DID

### Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/atchess.git
cd atchess

# Build both binaries
make build
```

### Configuration

Create a config file at `~/.config/atchess/config.yaml`:

```yaml
protocol:
  pds_host: "https://your-pds.example.com"
  did: "did:plc:youridentifier"
  
web:
  listen_addr: "127.0.0.1:8080"
  static_dir: "./static"
```

### Running

```bash
# Start the protocol service
./bin/atchess-protocol

# In another terminal, start the web server
./bin/atchess-web
```

Then open http://localhost:8080 in your browser.

## AT Protocol Data Model

ATChess uses the following collections:

### `app.chess.game`

Stores game metadata and current state:

```json
{
  "id": "game_12345",
  "white": "did:plc:player1",
  "black": "did:plc:player2",
  "status": "active",
  "currentFen": "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
  "pgn": "1. e4",
  "createdAt": "2025-07-08T12:34:56Z",
  "lastMoveAt": "2025-07-08T12:35:42Z",
  "timeControl": {
    "type": "fischer",
    "baseTimeSeconds": 600,
    "incrementSeconds": 5
  }
}
```

### `app.chess.move`

Individual moves within a game:

```json
{
  "gameId": "game_12345",
  "moveNumber": 1,
  "playerDid": "did:plc:player1",
  "san": "e4",
  "fen": "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
  "timestamp": "2025-07-08T12:35:42Z",
  "comment": "Opening with the King's pawn"
}
```

### `app.chess.challenge`

Game invitations:

```json
{
  "id": "challenge_67890",
  "challenger": "did:plc:player1",
  "recipient": "did:plc:player2",
  "status": "pending",
  "timeControl": {
    "type": "fischer",
    "baseTimeSeconds": 600,
    "incrementSeconds": 5
  },
  "createdAt": "2025-07-08T12:30:00Z"
}
```

## Development

### Project Structure

```
├── cmd/
│   ├── protocol/       # Protocol service entry point
│   └── web/            # Web application entry point
├── internal/
│   ├── atproto/        # AT Protocol interaction logic
│   ├── chess/          # Chess game logic
│   ├── config/         # Configuration handling
│   ├── lexicon/        # AT Protocol lexicon definitions
│   ├── repository/     # Data access layer
│   └── web/            # Web server and UI components
├── static/             # Static web assets
├── Makefile            # Build automation
└── README.md           # This file
```

### Building from Source

```bash
# Build everything
make

# Build just the protocol service
make protocol

# Build just the web application
make web

# Run tests
make test
```

## License

MIT License - See LICENSE file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

---

**Note:** ATChess is under active development. The AT Protocol is still evolving, so expect changes as the protocol matures.
