#!/bin/bash

# List all ATChess games on the development PDS
# This script queries the AT Protocol PDS for all game records

set -e

# Configuration
PDS_URL="http://localhost:3000"
COLLECTION="app.atchess.game"

# Test account credentials
PLAYER1_HANDLE="player1.test"
PLAYER1_PASSWORD="player1pass"
PLAYER2_HANDLE="player2.test"
PLAYER2_PASSWORD="player2pass"

echo "🏁 ATChess Game Listing Script"
echo "=============================="
echo "PDS URL: $PDS_URL"
echo "Collection: $COLLECTION"
echo ""

# Function to create session and get access token
create_session() {
    local handle=$1
    local password=$2
    
    curl -s -X POST "$PDS_URL/xrpc/com.atproto.server.createSession" \
        -H "Content-Type: application/json" \
        -d "{\"identifier\":\"$handle\",\"password\":\"$password\"}"
}

# Function to list records for a user
list_user_games() {
    local handle=$1
    local password=$2
    
    echo "🎮 Listing games for $handle..."
    
    # Create session
    session_response=$(create_session "$handle" "$password")
    
    # Check if session creation was successful
    if echo "$session_response" | jq -e '.error' >/dev/null 2>&1; then
        echo "❌ Failed to create session for $handle:"
        echo "$session_response" | jq '.message // .error'
        return 1
    fi
    
    # Extract session info
    access_jwt=$(echo "$session_response" | jq -r '.accessJwt')
    did=$(echo "$session_response" | jq -r '.did')
    
    if [ "$access_jwt" = "null" ] || [ "$did" = "null" ]; then
        echo "❌ Invalid session response for $handle"
        return 1
    fi
    
    echo "   DID: $did"
    
    # List records
    records_response=$(curl -s -X GET "$PDS_URL/xrpc/com.atproto.repo.listRecords?repo=$did&collection=$COLLECTION" \
        -H "Authorization: Bearer $access_jwt")
    
    # Check if request was successful
    if echo "$records_response" | jq -e '.error' >/dev/null 2>&1; then
        echo "❌ Failed to list records for $handle:"
        echo "$records_response" | jq '.message // .error'
        return 1
    fi
    
    # Parse and display games
    game_count=$(echo "$records_response" | jq '.records | length')
    echo "   Found $game_count games:"
    
    if [ "$game_count" -gt 0 ]; then
        echo "$records_response" | jq -r '.records[] | 
            "   📋 Game ID: " + .uri + 
            "\n      White: " + .value.white + 
            "\n      Black: " + .value.black + 
            "\n      Status: " + .value.status + 
            "\n      FEN: " + .value.fen + 
            "\n      Created: " + .value.createdAt + 
            "\n"'
    else
        echo "   No games found"
    fi
    
    echo ""
}

# Function to get all games summary
get_all_games_summary() {
    echo "📊 Summary of all games:"
    echo "======================"
    
    # Get all games from both players
    all_games_temp=$(mktemp)
    
    for handle in "$PLAYER1_HANDLE" "$PLAYER2_HANDLE"; do
        password="${handle/player1.test/$PLAYER1_PASSWORD}"
        password="${password/player2.test/$PLAYER2_PASSWORD}"
        
        session_response=$(create_session "$handle" "$password" 2>/dev/null)
        
        if echo "$session_response" | jq -e '.error' >/dev/null 2>&1; then
            continue
        fi
        
        access_jwt=$(echo "$session_response" | jq -r '.accessJwt')
        did=$(echo "$session_response" | jq -r '.did')
        
        if [ "$access_jwt" != "null" ] && [ "$did" != "null" ]; then
            records_response=$(curl -s -X GET "$PDS_URL/xrpc/com.atproto.repo.listRecords?repo=$did&collection=$COLLECTION" \
                -H "Authorization: Bearer $access_jwt" 2>/dev/null)
            
            if ! echo "$records_response" | jq -e '.error' >/dev/null 2>&1; then
                echo "$records_response" | jq -r '.records[] | .uri' >> "$all_games_temp"
            fi
        fi
    done
    
    # Sort and deduplicate
    unique_games=$(sort "$all_games_temp" | uniq)
    game_count=$(echo "$unique_games" | wc -l)
    
    if [ -s "$all_games_temp" ]; then
        echo "Total unique games: $game_count"
        echo ""
        echo "All game IDs:"
        echo "$unique_games" | while read -r game_id; do
            if [ -n "$game_id" ]; then
                # Create base64 encoded version for API testing
                encoded_id=$(echo -n "$game_id" | base64 | tr '+/' '-_')
                echo "  🎯 $game_id"
                echo "     Base64: $encoded_id"
                echo "     API URL: http://localhost:8080/api/games/$encoded_id"
                echo ""
            fi
        done
    else
        echo "No games found on PDS"
    fi
    
    rm -f "$all_games_temp"
}

# Main execution
echo "Checking PDS connection..."
if ! curl -s "$PDS_URL/xrpc/com.atproto.server.describeServer" >/dev/null; then
    echo "❌ Cannot connect to PDS at $PDS_URL"
    echo "Make sure the PDS is running with: docker-compose up -d"
    exit 1
fi

echo "✅ PDS connection successful"
echo ""

# List games for each player
list_user_games "$PLAYER1_HANDLE" "$PLAYER1_PASSWORD"
list_user_games "$PLAYER2_HANDLE" "$PLAYER2_PASSWORD"

# Show summary
get_all_games_summary

echo "🎉 Game listing complete!"
echo ""
echo "💡 Usage tips:"
echo "  - Use the Base64 encoded IDs to test the GET /api/games/{id} endpoint"
echo "  - Copy game IDs to test move endpoints"
echo "  - Check game status and FEN to understand current state"