# ATChess TODO

This document tracks remaining work needed to make ATChess a fully functional federated chess platform.

## Backend (Protocol Service)

### Critical Federation Issues
- [ ] **Implement challenge discovery mechanism**
  - [ ] Add firehose subscription for real-time challenge notifications
  - [ ] Implement `GET /api/challenges` endpoint to list incoming challenges
  - [ ] Add challenge acceptance/decline endpoints
  - [ ] Create background service to poll for challenges from known players

- [ ] **Fix cross-PDS game visibility**
  - [ ] Implement proper AT Protocol record querying across different PDSes
  - [ ] Add game discovery for opponents on different PDS instances
  - [ ] Handle authentication for cross-PDS record access

- [ ] **Real-time game updates**
  - [ ] Add WebSocket support for live move notifications
  - [ ] Implement game state synchronization between players
  - [ ] Add connection handling for players on different PDSes

### Game Logic Enhancements
- [ ] **Advanced chess features**
  - [ ] Implement castling validation and execution
  - [ ] Add en passant move support
  - [ ] Implement pawn promotion logic
  - [ ] Add proper check/checkmate detection beyond basic validation

- [ ] **Time controls**
  - [ ] Implement chess clocks (time tracking per player)
  - [ ] Add timeout handling (forfeit on time)
  - [ ] Support different time control formats (classical, rapid, blitz)

- [ ] **Game management**
  - [ ] Add game resignation endpoint
  - [ ] Implement draw offers and acceptance
  - [ ] Add game history and statistics tracking
  - [ ] Support game spectating (read-only access)

### AT Protocol Integration
- [ ] **Notification system**
  - [ ] Integrate with AT Protocol notifications when available
  - [ ] Add push notification support for mobile clients
  - [ ] Implement email notifications for game events

- [ ] **Enhanced federation**
  - [ ] Add proper DID resolution for cross-PDS players
  - [ ] Implement PDS discovery mechanisms
  - [ ] Add support for AT Protocol firehose consumption
  - [ ] Handle PDS connectivity issues gracefully

### Security & Performance
- [ ] **Authentication improvements**
  - [ ] Add session management and refresh tokens
  - [ ] Implement proper CORS handling for cross-origin requests
  - [ ] Add rate limiting for API endpoints

- [ ] **Performance optimization**
  - [ ] Add caching for frequently accessed game states
  - [ ] Implement database connection pooling
  - [ ] Add metrics and monitoring endpoints

## Web Frontend

### Authentication & User Management
- [ ] **OAuth integration**
  - [ ] Add OAuth flow for AT Protocol authentication
  - [ ] Implement secure token storage and management
  - [ ] Add user profile management interface
  - [ ] Support multiple AT Protocol providers

- [ ] **Session management**
  - [ ] Add persistent login sessions
  - [ ] Implement logout functionality
  - [ ] Add session timeout handling

### Game Interface Improvements
- [ ] **Enhanced chessboard**
  - [ ] Add piece animation for moves
  - [ ] Implement drag-and-drop piece movement
  - [ ] Add move highlighting and last move indication
  - [ ] Support board flipping for black player perspective

- [ ] **Game state visualization**
  - [ ] Add captured pieces display
  - [ ] Implement move history sidebar
  - [ ] Add game status indicators (check, checkmate, draw)
  - [ ] Display time controls and remaining time

- [ ] **Move input methods**
  - [ ] Support algebraic notation input
  - [ ] Add click-to-move as alternative to drag-and-drop
  - [ ] Implement move validation feedback
  - [ ] Add move suggestion/hint system

### User Experience
- [ ] **Game discovery and management**
  - [ ] Add game lobby for finding opponents
  - [ ] Implement challenge creation and management interface
  - [ ] Add active games list with quick access
  - [ ] Create game history and statistics dashboard

- [ ] **Real-time features**
  - [ ] Add live move updates via WebSocket
  - [ ] Implement chat system for players
  - [ ] Add spectator mode with live commentary
  - [ ] Show online status of known players

- [ ] **Mobile responsiveness**
  - [ ] Optimize chessboard for touch devices
  - [ ] Add mobile-friendly navigation
  - [ ] Implement swipe gestures for piece movement
  - [ ] Add progressive web app (PWA) support

### Settings & Preferences
- [ ] **Appearance customization**
  - [ ] Add multiple chessboard themes
  - [ ] Implement dark/light mode toggle
  - [ ] Support custom piece sets
  - [ ] Add board size and orientation preferences

- [ ] **Notification preferences**
  - [ ] Add in-app notification settings
  - [ ] Implement sound effects for moves and events
  - [ ] Support browser notifications for turn alerts

## Additional Features

### Tournament Support
- [ ] **Tournament system**
  - [ ] Add tournament creation and management
  - [ ] Implement bracket generation and tracking
  - [ ] Add tournament leaderboards and statistics
  - [ ] Support different tournament formats (Swiss, round-robin, knockout)

### Analysis Tools
- [ ] **Game analysis**
  - [ ] Add post-game analysis with engine evaluation
  - [ ] Implement position evaluation display
  - [ ] Add move analysis and suggestion system
  - [ ] Support PGN export and import

### Developer Tools
- [ ] **CLI interface**
  - [ ] Write a CLI tool to play games from command line
  - [ ] Add CLI support for challenge management
  - [ ] Implement automated testing tools
  - [ ] Add development utilities for PDS management

### Documentation & Community
- [ ] **Enhanced documentation**
  - [ ] Add API documentation with OpenAPI/Swagger
  - [ ] Create developer integration guides
  - [ ] Add federation best practices documentation
  - [ ] Write AT Protocol lexicon documentation

- [ ] **Community features**
  - [ ] Add player rating system (ELO)
  - [ ] Implement friend/following system
  - [ ] Add community tournaments and events
  - [ ] Create public game sharing and analysis

## Infrastructure & Deployment

### Production Readiness
- [ ] **Deployment automation**
  - [ ] Add Docker production configurations
  - [ ] Implement CI/CD pipelines
  - [ ] Add automated testing in CI
  - [ ] Create production deployment scripts

- [ ] **Monitoring & Observability**
  - [ ] Add structured logging throughout application
  - [ ] Implement metrics collection and dashboards
  - [ ] Add health checks and uptime monitoring
  - [ ] Create error tracking and alerting

- [ ] **Scalability**
  - [ ] Add horizontal scaling support
  - [ ] Implement database sharding strategies
  - [ ] Add load balancing configuration
  - [ ] Optimize for high-concurrency gameplay

---

## Priority Levels

**P0 (Critical)**: Challenge discovery, OAuth integration, basic game completion
**P1 (High)**: Real-time updates, enhanced game features, mobile responsiveness  
**P2 (Medium)**: Tournament support, analysis tools, CLI interface
**P3 (Low)**: Community features, advanced customization, production optimization

For detailed technical specifications and implementation notes, see the existing documentation in `/docs/`.