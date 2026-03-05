#!/bin/bash

# End-to-end test runner for ATChess
# This script runs the full e2e test suite with proper service orchestration

set -e

echo "🧪 Running ATChess End-to-End Tests"
echo "=================================="

# Check if PDS is running
if ! curl -f -s http://localhost:3000/xrpc/com.atproto.server.describeServer >/dev/null 2>&1; then
    echo "❌ PDS is not running. Please start it first:"
    echo "   docker-compose up -d"
    exit 1
fi

echo "✅ PDS is running"

# Check if protocol service is running
if ! curl -f -s http://localhost:8080/api/health >/dev/null 2>&1; then
    echo "❌ Protocol service is not running. Please start it first:"
    echo "   make run-protocol"
    exit 1
fi

echo "✅ Protocol service is running"

# Verify test accounts exist
echo "🔍 Verifying test accounts..."

# Test player1.test login
if ! curl -f -s -X POST http://localhost:3000/xrpc/com.atproto.server.createSession \
    -H "Content-Type: application/json" \
    -d '{"identifier": "player1.test", "password": "player1pass"}' >/dev/null 2>&1; then
    echo "❌ player1.test account not found. Please create test accounts:"
    echo "   ./scripts/create-test-accounts.sh"
    exit 1
fi

# Test player2.test login
if ! curl -f -s -X POST http://localhost:3000/xrpc/com.atproto.server.createSession \
    -H "Content-Type: application/json" \
    -d '{"identifier": "player2.test", "password": "player2pass"}' >/dev/null 2>&1; then
    echo "❌ player2.test account not found. Please create test accounts:"
    echo "   ./scripts/create-test-accounts.sh"
    exit 1
fi

echo "✅ Test accounts verified"

# Run the e2e tests
echo ""
echo "🚀 Running end-to-end tests..."
echo ""

# Run with verbose output and race detection
go test -v -race -tags=e2e -timeout 60s ./test/e2e/...

echo ""
echo "🎉 All end-to-end tests completed successfully!"
echo ""
echo "📋 Test Summary:"
echo "   ✅ Full game lifecycle (challenge → accept → moves → checkmate)"
echo "   ✅ Fool's mate (white wins): e4 e5 Qh5 Ke7 Qxe5#"
echo "   ✅ Scholar's mate variant (black wins): g4 e5 f4 Qh4#"
echo "   ✅ REST API endpoints tested"
echo "   ✅ AT Protocol integration verified"
echo ""
echo "💡 Next steps:"
echo "   - Run 'make run-web' to start the web interface"
echo "   - Open http://localhost:8081 to play interactively"
echo "   - Test with real moves on the visual chessboard"