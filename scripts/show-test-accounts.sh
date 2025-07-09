#!/bin/bash

# Show test account DIDs for ATChess development

echo "📋 ATChess Test Account DIDs"
echo "============================"

PDS_URL="http://localhost:3000"

# Check if PDS is running
if ! curl -f -s "$PDS_URL/xrpc/com.atproto.server.describeServer" >/dev/null 2>&1; then
    echo "❌ PDS is not running at $PDS_URL"
    echo "   Please start it with: docker-compose up -d"
    exit 1
fi

echo "✅ PDS is running"
echo ""

# Get Player 1 DID
echo "🔍 Getting Player 1 DID..."
PLAYER1_DID=$(curl -s -X POST "$PDS_URL/xrpc/com.atproto.server.createSession" \
  -H "Content-Type: application/json" \
  -d '{"identifier": "player1.test", "password": "player1pass"}' | jq -r '.did')

if [ "$PLAYER1_DID" = "null" ] || [ -z "$PLAYER1_DID" ]; then
    echo "❌ Failed to get Player 1 DID"
    exit 1
fi

# Get Player 2 DID
echo "🔍 Getting Player 2 DID..."
PLAYER2_DID=$(curl -s -X POST "$PDS_URL/xrpc/com.atproto.server.createSession" \
  -H "Content-Type: application/json" \
  -d '{"identifier": "player2.test", "password": "player2pass"}' | jq -r '.did')

if [ "$PLAYER2_DID" = "null" ] || [ -z "$PLAYER2_DID" ]; then
    echo "❌ Failed to get Player 2 DID"
    exit 1
fi

echo ""
echo "📋 Test Account Summary:"
echo "========================"
echo "Player 1 (player1.test):"
echo "  Handle: player1.test"
echo "  Password: player1pass"
echo "  DID: $PLAYER1_DID"
echo ""
echo "Player 2 (player2.test):"
echo "  Handle: player2.test"
echo "  Password: player2pass"
echo "  DID: $PLAYER2_DID"
echo ""
echo "🎯 API Usage Examples:"
echo "======================"
echo ""
echo "# Create a game (Player 1 as white vs Player 2):"
echo "curl -X POST http://localhost:8080/api/games \\"
echo "  -H \"Content-Type: application/json\" \\"
echo "  -d '{\"opponent_did\": \"$PLAYER2_DID\", \"color\": \"white\"}'"
echo ""
echo "# Create a game (Player 2 as white vs Player 1):"
echo "curl -X POST http://localhost:8080/api/games \\"
echo "  -H \"Content-Type: application/json\" \\"
echo "  -d '{\"opponent_did\": \"$PLAYER1_DID\", \"color\": \"white\"}'"
echo ""
echo "# Make a move (use game ID from create response):"
echo "curl -X POST http://localhost:8080/api/games/test-game/moves \\"
echo "  -H \"Content-Type: application/json\" \\"
echo "  -d '{\"from\": \"e2\", \"to\": \"e4\", \"fen\": \"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1\", \"game_id\": \"GAME_ID_HERE\"}'"