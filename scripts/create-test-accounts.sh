#!/bin/bash

set -e  # Exit on any error

PDS_URL="http://localhost:3000"

echo "🔍 Checking PDS availability..."

# Check if PDS is running
if ! curl -f -s "$PDS_URL/xrpc/com.atproto.server.describeServer" >/dev/null 2>&1; then
    echo "❌ PDS is not running or not accessible at $PDS_URL"
    echo "   Please start the PDS first with: docker-compose up -d"
    echo "   Wait for it to be ready, then try again."
    exit 1
fi

echo "✅ PDS is running"
echo ""
echo "👥 Creating test accounts..."

# Function to create account with error handling
create_account() {
    local email=$1
    local handle=$2
    local password=$3
    local account_name=$4
    
    echo "📝 Creating $account_name ($handle)..."
    
    response=$(curl -s -w "%{http_code}" -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
        -H "Content-Type: application/json" \
        -d "{
            \"email\": \"$email\",
            \"handle\": \"$handle\",
            \"password\": \"$password\",
            \"inviteCode\": \"\"
        }")
    
    http_code="${response: -3}"
    response_body="${response%???}"
    
    if [ "$http_code" = "200" ]; then
        echo "✅ $account_name created successfully"
        # Extract and display DID
        did=$(echo "$response_body" | grep -o '"did":"[^"]*"' | cut -d'"' -f4)
        if [ -n "$did" ]; then
            echo "   DID: $did"
        fi
    elif [ "$http_code" = "400" ] && echo "$response_body" | grep -q "Handle already taken"; then
        echo "ℹ️  $account_name already exists"
    else
        echo "❌ Failed to create $account_name (HTTP $http_code)"
        echo "   Response: $response_body"
        return 1
    fi
    echo ""
}

# Create Player 1
if ! create_account "player1@chess.test" "player1.test" "player1pass" "Player 1"; then
    echo "❌ Failed to create Player 1"
    exit 1
fi

# Create Player 2
if ! create_account "player2@chess.test" "player2.test" "player2pass" "Player 2"; then
    echo "❌ Failed to create Player 2"
    exit 1
fi

echo "🎉 Test accounts setup complete!"
echo ""
echo "📋 Account Summary:"
echo "   Player 1: player1.test (password: player1pass)"
echo "   Player 2: player2.test (password: player2pass)"
echo ""
echo "🔑 To get DIDs for testing:"
echo "   curl -X POST $PDS_URL/xrpc/com.atproto.server.createSession \\"
echo "     -H \"Content-Type: application/json\" \\"
echo "     -d '{\"identifier\": \"player1.test\", \"password\": \"player1pass\"}'"