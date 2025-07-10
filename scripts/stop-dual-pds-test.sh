#!/bin/bash

echo "🛑 Stopping Dual PDS Test Environment"
echo "====================================="
echo ""

# PID file location
PID_FILE=".atchess-test-pids"

# Function to check if a PID is still running
is_pid_running() {
    local pid=$1
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        return 0
    else
        return 1
    fi
}

# Stop ATChess services
echo "1️⃣ Stopping ATChess services..."

if [ -f "$PID_FILE" ]; then
    source "$PID_FILE"
    
    if is_pid_running "$PROTOCOL_PID"; then
        echo "   Stopping protocol service (PID: $PROTOCOL_PID)..."
        kill "$PROTOCOL_PID" 2>/dev/null
        echo "   ✅ Protocol service stopped"
    else
        echo "   ℹ️  Protocol service not running or already stopped"
    fi
    
    if is_pid_running "$WEB_PID"; then
        echo "   Stopping web service (PID: $WEB_PID)..."
        kill "$WEB_PID" 2>/dev/null
        echo "   ✅ Web service stopped"
    else
        echo "   ℹ️  Web service not running or already stopped"
    fi
    
    # Clean up PID file
    rm -f "$PID_FILE"
    echo "   ✅ PID file cleaned up"
else
    echo "   ⚠️  No PID file found. Services may not be managed by test script."
    echo "   Checking for any running ATChess processes..."
    
    # Try to find and stop any ATChess processes
    if pgrep -f "atchess-protocol" > /dev/null; then
        echo "   Found ATChess protocol service, stopping..."
        pkill -f "atchess-protocol"
    fi
    
    if pgrep -f "atchess-web" > /dev/null; then
        echo "   Found ATChess web service, stopping..."
        pkill -f "atchess-web"
    fi
fi

echo ""
echo "2️⃣ Stopping dual PDS containers..."

# Check if docker-compose file exists
if [ -f "docker-compose.dual-pds.yml" ]; then
    if docker-compose -f docker-compose.dual-pds.yml ps | grep -q "Up"; then
        docker-compose -f docker-compose.dual-pds.yml down
        echo "   ✅ Dual PDS containers stopped"
    else
        echo "   ℹ️  Dual PDS containers not running"
    fi
else
    echo "   ❌ docker-compose.dual-pds.yml not found"
fi

echo ""
echo "3️⃣ Cleanup complete!"
echo ""
echo "📋 Summary:"
echo "   - ATChess services stopped"
echo "   - PID tracking file removed"
echo "   - Docker containers stopped"
echo ""
echo "💡 To remove Docker volumes and start completely fresh:"
echo "   docker-compose -f docker-compose.dual-pds.yml down -v"
echo ""
echo "🚀 To start again:"
echo "   ./scripts/test-dual-pds-setup.sh"