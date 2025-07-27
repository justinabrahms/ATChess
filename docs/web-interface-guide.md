# ATChess Web Interface Guide

## Overview

The ATChess web interface provides a complete chess playing experience integrated with the AT Protocol (Bluesky). Users can log in with their Bluesky accounts, create games, challenge other players, and play chess in real-time.

## Getting Started

### 1. Prerequisites

- A Bluesky account
- An app password from Bluesky settings

### 2. Creating an App Password

1. Go to [Bluesky Settings](https://bsky.app/settings/app-passwords)
2. Click "Add App Password"
3. Give it a name like "ATChess"
4. Copy the generated password (format: xxxx-xxxx-xxxx-xxxx)

### 3. Logging In

1. Navigate to the ATChess web interface (e.g., http://localhost:8081)
2. Enter your Bluesky handle (e.g., yourname.bsky.social)
3. Enter the app password you created
4. Click "Login with Bluesky"

## Playing Chess

### Creating a New Game

1. In the sidebar, find the "Create New Game" section
2. Enter your opponent's Bluesky handle
3. Choose your preferred color (White, Black, or Random)
4. Click "Create Game"

This sends a challenge to your opponent that they can accept or decline.

### Accepting Challenges

1. Check the "Challenges" section in the sidebar
2. Review incoming challenges showing who sent them and what color they want to play
3. Click "Accept" to start the game or "Decline" to reject

### Playing Moves

1. When it's your turn, click on a piece to select it
2. Click on a valid destination square to move
3. The board updates automatically
4. Your opponent's moves appear in real-time via WebSocket

### Game Actions

During an active game, you can:
- **Offer Draw**: Propose to end the game in a draw
- **Resign**: Concede the game to your opponent

## Features

### Real-time Updates
- Moves are synchronized instantly between players
- Game status updates automatically
- Connection status indicator shows WebSocket state

### Game Persistence
- Games are stored in your AT Protocol repository
- Share game URLs to let others spectate
- Resume games anytime by loading the URL

### Clean Interface
- Modern, responsive design
- Clear game status indicators
- Intuitive drag-and-drop piece movement
- Mobile-friendly layout

## Technical Details

### Authentication
- Uses AT Protocol authentication with DPoP support
- Session data stored in localStorage
- No passwords are stored, only session tokens

### Data Storage
- All game data stored in AT Protocol repositories
- Games belong to both players for redundancy
- Moves are validated server-side before storage

### API Endpoints

The web interface communicates with these endpoints:
- `POST /api/auth/login` - Authenticate with Bluesky
- `POST /api/games` - Create a new game
- `GET /api/games/{id}` - Load game state
- `POST /api/moves` - Submit a move
- `POST /api/challenges` - Send a challenge
- `GET /api/challenge-notifications` - Get pending challenges
- WebSocket `/api/ws` - Real-time game updates

## Troubleshooting

### Can't Log In
- Verify your handle is correct (include .bsky.social)
- Make sure you're using an app password, not your main password
- Check that the protocol service is running

### Moves Not Working
- Ensure it's your turn
- Verify the move is legal
- Check the connection status indicator

### Can't Find Opponent
- Opponent must have a Bluesky account
- Handle must be exact (case-sensitive)
- They need to accept the challenge to start playing

## Development

To run the web interface locally:

```bash
# Start the protocol service (required)
make dev-protocol

# In another terminal, start the web service
make dev-web

# Navigate to http://localhost:8081
```

The web interface uses vanilla JavaScript with no framework dependencies, making it lightweight and easy to modify.