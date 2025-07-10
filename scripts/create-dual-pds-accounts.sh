#!/bin/bash

set -e  # Exit on any error

PDS_USER3_URL="http://localhost:3002"
PDS_USER4_URL="http://localhost:3003"

echo "🔍 Checking dual PDS availability..."

# Check if both PDSes are running
check_pds() {
    local url=$1
    local name=$2
    
    if ! curl -f -s "$url/xrpc/com.atproto.server.describeServer" >/dev/null 2>&1; then
        echo "❌ $name is not running or not accessible at $url"
        echo "   Please start the dual PDS setup first with: docker-compose -f docker-compose.dual-pds.yml up -d"
        echo "   Wait for both PDSes to be ready, then try again."
        exit 1
    fi
    echo "✅ $name is running at $url"
}

check_pds "$PDS_USER3_URL" "PDS for user3"
check_pds "$PDS_USER4_URL" "PDS for user4"

echo ""
echo "👥 Creating cross-PDS test accounts..."

# Function to create account with error handling
create_account() {
    local pds_url=$1
    local email=$2
    local handle=$3
    local password=$4
    local account_name=$5
    
    echo "📝 Creating $account_name ($handle) on $pds_url..."
    
    response=$(curl -s -w "%{http_code}" -X POST "$pds_url/xrpc/com.atproto.server.createAccount" \
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
            echo "   PDS: $pds_url"
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

# Create user3 on first PDS (port 3000)
if ! create_account "$PDS_USER3_URL" "user3@chess.test" "user3.test" "user3pass" "User 3"; then
    echo "❌ Failed to create User 3"
    exit 1
fi

# Create user4 on second PDS (port 3001)
if ! create_account "$PDS_USER4_URL" "user4@chess.test" "user4.test" "user4pass" "User 4"; then
    echo "❌ Failed to create User 4"
    exit 1
fi

echo "🎉 Cross-PDS test accounts setup complete!"
echo ""
echo "📋 Account Summary:"
echo "   User 3: user3.test (password: user3pass) on PDS $PDS_USER3_URL"
echo "   User 4: user4.test (password: user4pass) on PDS $PDS_USER4_URL"
echo ""
echo "🔑 To get DIDs and test cross-PDS functionality:"
echo ""
echo "# Get User 3 session:"
echo "curl -X POST $PDS_USER3_URL/xrpc/com.atproto.server.createSession \\"
echo "  -H \"Content-Type: application/json\" \\"
echo "  -d '{\"identifier\": \"user3.test\", \"password\": \"user3pass\"}'"
echo ""
echo "# Get User 4 session:"
echo "curl -X POST $PDS_USER4_URL/xrpc/com.atproto.server.createSession \\"
echo "  -H \"Content-Type: application/json\" \\"
echo "  -d '{\"identifier\": \"user4.test\", \"password\": \"user4pass\"}'"
echo ""
echo "🎮 Now you can test cross-PDS chess games between user3 and user4!"
echo "   This setup will help identify any same-PDS vs cross-PDS protocol issues."