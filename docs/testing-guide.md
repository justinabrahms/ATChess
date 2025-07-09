# Testing Guide

This guide explains how to test the ATChess implementation with two accounts making moves against each other.

## Prerequisites

1. **Local PDS Running**: Follow the [Local PDS Setup Guide](local-pds-setup.md) to start your local PDS
2. **Test Accounts Created**: Use the provided script to create test accounts
3. **ATChess Services Built**: Run `make build` to build the services

## Quick Test Setup

### 1. Start Local PDS

```bash
# Start the PDS
docker-compose up -d

# Wait for it to be healthy
docker-compose ps
```

### 2. Create Test Accounts

```bash
# Create two test accounts
./scripts/create-test-accounts.sh
```

This creates:
- `player1.localhost` with password `player1pass`
- `player2.localhost` with password `player2pass`

### 3. Start ATChess Services

```bash
# Terminal 1: Start the protocol service
make run-protocol

# Terminal 2: Start the web interface
make run-web
```

The protocol service runs on `localhost:8080` and the web interface on `localhost:8081`.

## Testing Two-Player Game

### Manual Testing via Web Interface

1. **Open Web Interface**: Navigate to `http://localhost:8081`

2. **Create a Game**:
   - Enter opponent DID (you'll need to get this from the PDS)
   - Choose your color (white/black/random)
   - Click "Create Game"

3. **Get Player DIDs**:
   ```bash
   # Get player1 DID
   curl -X POST http://localhost:3000/xrpc/com.atproto.server.createSession \
     -H "Content-Type: application/json" \
     -d '{"identifier": "player1.localhost", "password": "player1pass"}'
   
   # Get player2 DID
   curl -X POST http://localhost:3000/xrpc/com.atproto.server.createSession \
     -H "Content-Type: application/json" \
     -d '{"identifier": "player2.localhost", "password": "player2pass"}'
   ```

### API Testing

#### 1. Test Game Creation

```bash
# Create a game between player1 and player2
curl -X POST http://localhost:8080/api/games \
  -H "Content-Type: application/json" \
  -d '{
    "opponent_did": "did:plc:player2-did-here",
    "color": "white"
  }'
```

#### 2. Test Move Submission

```bash
# Make a move (e.g., e2 to e4)
curl -X POST http://localhost:8080/api/games/GAME_ID/moves \
  -H "Content-Type: application/json" \
  -d '{
    "from": "e2",
    "to": "e4",
    "fen": "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
  }'
```

#### 3. Test Challenge Creation

```bash
# Create a challenge
curl -X POST http://localhost:8080/api/challenges \
  -H "Content-Type: application/json" \
  -d '{
    "opponent_did": "did:plc:player2-did-here",
    "color": "white",
    "message": "Want to play a game?"
  }'
```

## Automated Testing

### Unit Tests

```bash
# Run all tests
make test

# Run just chess engine tests
make test-protocol

# Run with verbose output
go test -v ./internal/chess/...
```

### Integration Tests

```bash
# Run integration tests
make test-integration

# Or manually
go test -v -tags=integration ./test/integration/...
```

## Test Scenarios

### Basic Game Flow

1. **Create Game**: Player1 creates a game challenging Player2
2. **Make Moves**: Alternating moves between players
3. **Validate Moves**: Each move is validated by the chess engine
4. **Record in AT Protocol**: Moves are stored in both players' repositories

### Sample Game Sequence

```bash
# Player1 starts (white)
# e2-e4 (King's pawn opening)
curl -X POST http://localhost:8080/api/games/GAME_ID/moves \
  -H "Content-Type: application/json" \
  -d '{"from": "e2", "to": "e4", "fen": "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"}'

# Player2 responds (black)  
# e7-e5 (King's pawn defense)
curl -X POST http://localhost:8080/api/games/GAME_ID/moves \
  -H "Content-Type: application/json" \
  -d '{"from": "e7", "to": "e5", "fen": "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"}'

# Player1 develops
# g1-f3 (Knight development)
curl -X POST http://localhost:8080/api/games/GAME_ID/moves \
  -H "Content-Type: application/json" \
  -d '{"from": "g1", "to": "f3", "fen": "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq e6 0 2"}'
```

## Troubleshooting

### Common Issues

1. **PDS Not Running**: Check `docker-compose ps` and restart if needed
2. **Account Creation Fails**: Ensure PDS is fully started and healthy
3. **Move Validation Fails**: Check FEN notation is correct and move is legal
4. **AT Protocol Errors**: Verify account credentials and PDS connectivity

### Debug Commands

```bash
# Check PDS health
curl http://localhost:3000/_health

# Check ATChess protocol service
curl http://localhost:8080/api/health

# View PDS logs
docker-compose logs -f pds

# Check created accounts
curl http://localhost:3000/xrpc/com.atproto.sync.listRepos
```

### Reset Test Environment

```bash
# Stop all services
docker-compose down

# Remove all data
docker-compose down -v

# Restart fresh
docker-compose up -d
./scripts/create-test-accounts.sh
```

## Expected Results

After successful testing, you should see:

1. **Game Records**: Created in both players' AT Protocol repositories
2. **Move Records**: Each move stored with metadata (SAN, FEN, timestamps)
3. **Challenge Records**: Game invitations properly federated
4. **Web Interface**: Interactive chessboard responding to moves
5. **Validation**: Invalid moves rejected with appropriate errors

## Next Steps

Once basic two-player testing works:

1. **Add Real-time Updates**: WebSocket support for live game updates
2. **Implement Check Detection**: Proper check/checkmate detection
3. **Add Time Controls**: Implement chess clocks
4. **Cross-PDS Testing**: Test games between different PDS instances
5. **Mobile Interface**: Responsive design for mobile devices
6. **Tournament Support**: Multi-player tournament brackets