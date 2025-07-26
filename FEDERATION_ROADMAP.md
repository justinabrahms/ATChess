# ATChess Federation & Firehose Integration Roadmap

## Overview

This document outlines the technical plan to transform ATChess from a direct-API chess application into a fully federated, real-time chess platform using AT Protocol's firehose and OAuth capabilities. The goal is to enable users to authenticate with their AT Protocol accounts, play chess games in real-time, and allow spectators to watch games as they unfold.

## Current State Analysis

### What Works
- Basic chess game creation and move validation
- Game state stored in both players' PDS repositories
- Web UI for playing games
- Direct HTTP API calls to AT Protocol

### Critical Limitations
1. **No Challenge Discovery**: Challenges are only stored in the challenger's repo, making them invisible to challenged players via firehose
2. **No Firehose Integration**: Missing real-time updates for moves and game state changes
3. **No OAuth**: Currently uses username/password authentication
4. **No Spectator Support**: No mechanism for third parties to discover and watch games

## Implementation Plan

### Phase 1: Challenge Discovery Fix

**Goal**: Enable players to discover when they've been challenged to a game

**Tasks**:
1. Create new lexicon: `app.atchess.challengeNotification`
   - Stored in the challenged player's repository
   - Contains reference to the original challenge
   - Allows firehose discovery of incoming challenges

2. Update challenge creation flow:
   - When creating a challenge, also create a notification in the challenged player's repo
   - Handle cases where write access is denied (privacy settings)
   - Implement fallback notification mechanisms

3. Create challenge inbox UI:
   - List pending challenges with time control info
   - Accept/decline functionality
   - Real-time updates via firehose
   - Display time control (e.g., "1-3 days per move")

**Files to modify**:
- `lexicons/app.atchess.challengeNotification.json` (new)
- `internal/atproto/client.go` - Add `CreateChallengeNotification()` method
- `internal/web/service.go` - Update challenge creation endpoint
- `web/static/index.html` - Add challenge inbox UI

**Testing Requirements**:
- Unit tests for challenge notification creation
- Integration tests with dual PDS setup
- Test notification creation failure handling (privacy settings)
- E2E test: User A challenges User B, User B sees notification
- Test challenge expiration and cleanup

### Phase 2: Firehose Integration

**Goal**: Real-time game updates and move notifications

**Tasks**:
1. Implement firehose client:
   - WebSocket connection to `com.atproto.sync.subscribeRepos`
   - Message parsing and filtering for `app.atchess.*` records
   - Reconnection logic and error handling

2. Create event processing system:
   - Process incoming moves in real-time
   - Update local game state
   - Notify connected web clients via WebSocket

3. Add WebSocket support to web UI:
   - Server-Sent Events or WebSocket connection
   - Real-time board updates
   - Move notifications

4. Implement core chess mechanics:
   - Draw offers and acceptance
   - Resignation with confirmation dialog
   - Automatic draw detection (threefold repetition, 50-move rule)
   - Insufficient material detection

**Files to create**:
- `internal/firehose/client.go` - Firehose subscription client
- `internal/firehose/processor.go` - Event processing logic
- `internal/web/websocket.go` - WebSocket handler for web clients
- `lexicons/app.atchess.drawOffer.json` - Draw offer records
- `lexicons/app.atchess.resignation.json` - Resignation records

**Files to modify**:
- `cmd/protocol/main.go` - Initialize firehose connection
- `web/static/index.html` - Add WebSocket client code
- `internal/chess/engine.go` - Add draw detection logic

**Testing Requirements**:
- Unit tests for firehose message parsing
- Mock firehose for testing event processing
- Integration test with real firehose connection
- Test reconnection logic and error recovery
- Performance test: Handle 1000 moves/second
- E2E test: Move made on PDS A appears in real-time on client B

### Phase 3: DPoP Authentication Implementation

**Goal**: Implement AT Protocol's DPoP-based authentication system

**Tasks**:
1. Implement DPoP (Demonstrating Proof of Possession):
   - Generate ES256 (ECDSA P-256) key pairs
   - Create DPoP proof JWTs for each request
   - Bind access tokens to DPoP keys
   - Handle key rotation and storage

2. Update authentication flow:
   - Keep username/password login for now
   - Add DPoP proof generation to session creation
   - Include DPoP headers in all authenticated requests
   - Implement proper JWT handling with `ath` claim

3. Future auth considerations:
   - Plan for AT Protocol's upcoming OAuth-like delegation
   - Support app passwords as alternative
   - Prepare for "Login with AT Protocol" when spec is finalized

**Files to create**:
- `internal/auth/dpop.go` - DPoP key generation and proof creation
- `internal/auth/session.go` - Session management with DPoP
- `internal/auth/jwt.go` - JWT creation and validation helpers

**Files to modify**:
- `internal/atproto/client.go` - Add DPoP to all requests
- `internal/web/service.go` - Update auth endpoints
- `internal/config/config.go` - Add DPoP configuration

**Testing Requirements**:
- Unit tests for DPoP proof generation
- Test key pair generation and JWK formatting
- Verify JWT structure and claims (`jti`, `htm`, `htu`, `iat`, `ath`)
- Test token binding with access token hash
- Integration test with real AT Protocol PDS
- Security test: Ensure private keys never leak
- Test replay attack prevention (unique `jti`)
- E2E test: Complete auth flow with DPoP-protected API calls

### Phase 4: Spectator Mode

**Goal**: Allow anyone to discover and watch ongoing games

**Tasks**:
1. Create game discovery lexicon:
   - `app.atchess.gameIndex` - Public index of active games
   - Include player handles, ELO ratings, time controls

2. Implement game broadcasting:
   - Public game records with spectator access
   - Real-time move updates via firehose
   - Spectator count tracking

3. Build spectator UI:
   - Game browser/lobby
   - Live game viewing
   - Move history navigation
   - Material count display
   - Basic position evaluation

4. Add abandonment detection:
   - Track last move timestamp
   - Configurable timeout (e.g., 3 days for correspondence)
   - Allow opponent to claim victory after timeout
   - Automatic game conclusion after extended abandonment

**Files to create**:
- `lexicons/app.atchess.gameIndex.json` - Game discovery lexicon
- `internal/web/spectator.go` - Spectator endpoints
- `web/static/spectator.html` - Spectator UI

**Testing Requirements**:
- Unit tests for game indexing logic
- Test game discovery across multiple PDS instances
- Test spectator permissions (read-only access)
- Performance test: 100+ spectators on single game
- Integration test: Spectators receive real-time updates
- E2E test: Find active game → Watch → See moves in real-time

### Phase 5: Time Control Enforcement

**Goal**: Enforce the time controls specified during challenge creation

**Tasks**:
1. Implement correspondence time tracking:
   - Track time since last move
   - Support "days per move" format (1-3 days typical)
   - Send notifications when time is running low
   - Automatic forfeit on time expiry

2. Time violation handling:
   - Grace period for first offense
   - Clear UI indicators for time remaining
   - Option to claim victory on opponent timeout

**Testing Requirements**:
- Unit tests for clock calculations
- Integration tests for time forfeit scenarios
- Load tests for clock sync performance

### Future Enhancements (Post-MVP)

These features are intentionally deferred to focus on core federation functionality:

1. **ELO Rating System**:
   - Player ratings and matchmaking
   - Rating history and statistics
   - Skill-based game discovery

2. **Tournament Support**:
   - Swiss/round-robin formats
   - Automated pairings
   - Tournament lobbies and brackets

3. **Advanced Analysis**:
   - Engine integration for computer analysis
   - Opening book database
   - Automated game annotations

## Technical Considerations

### Firehose Subscription
```go
// Example firehose connection
type FirehoseClient struct {
    wsURL    string
    filters  []string // ["app.atchess.move", "app.atchess.challenge"]
    handlers map[string]EventHandler
}

// Subscribe to specific collections
client.Subscribe("app.atchess.*", handleChessEvent)
```

### DPoP Authentication
```go
// DPoP proof generation for each request
type DPoPManager struct {
    privateKey *ecdsa.PrivateKey
    publicJWK  map[string]interface{}
}

// Create proof for each API request
func (d *DPoPManager) CreateProof(method, uri, accessToken string) string {
    // Generate JWT with claims: jti, htm, htu, iat, ath
    // Sign with ES256 private key
    // Include JWK in header
}
```

### Challenge Notification Schema
```json
{
  "lexicon": 1,
  "id": "app.atchess.challengeNotification",
  "defs": {
    "main": {
      "type": "record",
      "description": "Notification of incoming chess challenge",
      "key": "tid",
      "record": {
        "type": "object",
        "required": ["createdAt", "challenge", "challenger"],
        "properties": {
          "createdAt": {"type": "string", "format": "datetime"},
          "challenge": {
            "type": "ref",
            "ref": "com.atproto.repo.strongRef",
            "description": "Reference to original challenge"
          },
          "challenger": {
            "type": "string",
            "format": "did",
            "description": "DID of the challenging player"
          },
          "timeControl": {
            "type": "object",
            "properties": {
              "daysPerMove": {
                "type": "integer",
                "description": "Days allowed per move (1-7)"
              },
              "type": {
                "type": "string",
                "enum": ["correspondence"],
                "description": "Time control type"
              }
            }
          },
          "expiresAt": {"type": "string", "format": "datetime"}
        }
      }
    }
  }
}
```

## Testing Strategy

### Test Infrastructure
1. **Dual PDS Testing**: Docker Compose setup with two PDS instances
2. **Firehose Simulator**: Mock firehose server for unit tests
3. **OAuth Mock Server**: Simulate AT Protocol OAuth provider
4. **Load Testing**: K6 or similar for performance testing

### Test Coverage Requirements
- Unit test coverage: Minimum 80% for core logic
- Integration tests for all cross-service interactions
- E2E tests for critical user journeys
- Performance benchmarks for each phase

### Continuous Integration
- Run all tests on every PR
- Nightly integration tests with real PDS
- Weekly load tests to catch performance regressions

## Success Metrics

- [ ] Players can discover challenges without polling
- [ ] Moves appear in real-time (< 100ms latency)
- [ ] OAuth login works with any AT Protocol provider
- [ ] Spectators can find and watch games
- [ ] System handles 1000+ concurrent games

## Implementation Order

1. Fix challenge discovery (Phase 1) - **Critical**
2. Add firehose support (Phase 2) - **Critical**
3. Implement OAuth (Phase 3) - **Important**
4. Add spectator mode (Phase 4) - **Nice to have**
5. Enhanced features (Phase 5) - **Future**

## Resources

- [AT Protocol Firehose Docs](https://atproto.com/specs/event-stream)
- [AT Protocol OAuth Spec](https://atproto.com/specs/oauth)
- [WebSocket API Reference](https://developer.mozilla.org/en/docs/Web/API/WebSocket)
- [Chess.js Library](https://github.com/jhlywa/chess.js) (for client-side validation)