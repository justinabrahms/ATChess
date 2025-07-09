#!/bin/bash

PDS_URL="http://localhost:3000"
ADMIN_PASSWORD="admin"

# Create two test accounts for chess games
echo "Creating test accounts..."

# Player 1
curl -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "player1@chess.test",
    "handle": "player1.localhost",
    "password": "player1pass",
    "inviteCode": ""
  }'

echo ""

# Player 2  
curl -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "player2@chess.test", 
    "handle": "player2.localhost",
    "password": "player2pass",
    "inviteCode": ""
  }'

echo ""
echo "Test accounts created!"