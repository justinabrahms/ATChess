#!/bin/bash

# ATChess Quick Start Script
# This script sets up a complete development environment in one command

set -e

echo "🚀 ATChess Quick Start"
echo "======================"

# Check prerequisites
echo "📋 Checking prerequisites..."

if ! command -v docker &> /dev/null; then
    echo "❌ Docker is required but not installed. Please install Docker first."
    exit 1
fi

if ! command -v docker-compose &> /dev/null; then
    echo "❌ Docker Compose is required but not installed. Please install Docker Compose first."
    exit 1
fi

if ! command -v go &> /dev/null; then
    echo "❌ Go is required but not installed. Please install Go 1.21+ first."
    exit 1
fi

if ! command -v make &> /dev/null; then
    echo "❌ Make is required but not installed. Please install Make first."
    exit 1
fi

echo "✅ All prerequisites found!"

# Build the project
echo "🔨 Building ATChess..."
make build

# Start PDS
echo "🐳 Starting local AT Protocol server..."
docker-compose up -d

# Wait for PDS to be ready
echo "⏳ Waiting for PDS to be ready..."
max_attempts=30
attempt=0
while [ $attempt -lt $max_attempts ]; do
    if curl -f http://localhost:3000/_health &> /dev/null; then
        echo "✅ PDS is ready!"
        break
    fi
    attempt=$((attempt + 1))
    sleep 2
done

if [ $attempt -eq $max_attempts ]; then
    echo "❌ PDS failed to start within 60 seconds"
    exit 1
fi

# Create test accounts
echo "👥 Creating test accounts..."
./scripts/create-test-accounts.sh

# Start ATChess services
echo "🎯 Starting ATChess services..."
echo "   - Protocol service: http://localhost:8080"
echo "   - Web interface: http://localhost:8081"

# Kill any existing processes on these ports
lsof -ti:8080 | xargs kill -9 2>/dev/null || true
lsof -ti:8081 | xargs kill -9 2>/dev/null || true

# Start services in background
make run-protocol &
PROTOCOL_PID=$!

make run-web &
WEB_PID=$!

# Wait for services to start
sleep 3

# Check if services are running
if ! curl -f http://localhost:8080/api/health &> /dev/null; then
    echo "❌ Protocol service failed to start"
    kill $PROTOCOL_PID $WEB_PID 2>/dev/null || true
    exit 1
fi

if ! curl -f http://localhost:8081 &> /dev/null; then
    echo "❌ Web service failed to start"
    kill $PROTOCOL_PID $WEB_PID 2>/dev/null || true
    exit 1
fi

echo ""
echo "🎉 ATChess is ready!"
echo "==================="
echo ""
echo "📱 Open your browser to: http://localhost:8081"
echo ""
echo "🧪 Test accounts created:"
echo "   - player1.localhost (password: player1pass)"
echo "   - player2.localhost (password: player2pass)"
echo ""
echo "📖 Next steps:"
echo "   1. Open http://localhost:8081 in your browser"
echo "   2. Get player DIDs from the testing guide"
echo "   3. Create a game and start playing!"
echo ""
echo "📚 Documentation:"
echo "   - Testing guide: docs/testing-guide.md"
echo "   - Development guide: CLAUDE.md"
echo ""
echo "🛑 To stop all services:"
echo "   - Press Ctrl+C to stop ATChess services"
echo "   - Run 'docker-compose down' to stop PDS"
echo ""

# Keep script running and handle cleanup
cleanup() {
    echo ""
    echo "🛑 Shutting down ATChess..."
    kill $PROTOCOL_PID $WEB_PID 2>/dev/null || true
    echo "✅ ATChess services stopped"
    echo "💡 Run 'docker-compose down' to stop the PDS if needed"
}

trap cleanup EXIT

# Wait for user interrupt
echo "⌨️  Press Ctrl+C to stop all services"
wait