#!/bin/bash

# ATChess Docker Troubleshooting Script
# Run this script if you're having Docker issues

echo "🔍 ATChess Docker Troubleshooting"
echo "================================="

# Check Docker installation
echo "📋 Checking Docker installation..."
if ! command -v docker &> /dev/null; then
    echo "❌ Docker is not installed or not in PATH"
    echo "   Install Docker from: https://docs.docker.com/get-docker/"
    exit 1
fi

if ! command -v docker-compose &> /dev/null; then
    echo "❌ Docker Compose is not installed or not in PATH"
    echo "   Install Docker Compose from: https://docs.docker.com/compose/install/"
    exit 1
fi

echo "✅ Docker and Docker Compose found"

# Check Docker daemon
echo ""
echo "🐳 Checking Docker daemon..."
if ! docker info &> /dev/null; then
    echo "❌ Docker daemon is not running"
    echo "   Start Docker Desktop (macOS/Windows) or run 'sudo systemctl start docker' (Linux)"
    exit 1
fi

echo "✅ Docker daemon is running"

# Check Docker memory and disk space
echo ""
echo "💾 Checking Docker resources..."
docker system df

# Check network connectivity
echo ""
echo "🌐 Checking network connectivity..."
if ! curl -s --connect-timeout 5 https://ghcr.io > /dev/null; then
    echo "❌ Cannot reach GitHub Container Registry (ghcr.io)"
    echo "   Check your internet connection and proxy settings"
else
    echo "✅ Network connectivity to ghcr.io is working"
fi

# Try to pull the PDS image
echo ""
echo "📥 Testing PDS image pull..."
if docker pull ghcr.io/bluesky-social/pds:latest &> /dev/null; then
    echo "✅ Successfully pulled ghcr.io/bluesky-social/pds:latest"
elif docker pull ghcr.io/bluesky-social/pds:0.4 &> /dev/null; then
    echo "✅ Successfully pulled ghcr.io/bluesky-social/pds:0.4"
    echo "   (Using version 0.4 instead of latest)"
else
    echo "❌ Failed to pull PDS image"
    echo "   Trying alternative troubleshooting..."
    
    # Check Docker Hub login status
    echo ""
    echo "🔐 Checking Docker authentication..."
    docker login --help > /dev/null 2>&1
    
    # Check available disk space
    echo ""
    echo "💽 Checking disk space..."
    df -h $(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo '/var/lib/docker')
    
    echo ""
    echo "🛠️  Suggested fixes:"
    echo "   1. Restart Docker: 'sudo systemctl restart docker' (Linux) or restart Docker Desktop"
    echo "   2. Clean up Docker: 'docker system prune -af'"
    echo "   3. Free up disk space if low"
    echo "   4. Check Docker Desktop memory allocation (increase to 4GB+)"
    echo "   5. Try again later if registry is temporarily unavailable"
fi

# Check port availability
echo ""
echo "🔌 Checking port availability..."
if lsof -i :3000 &> /dev/null; then
    echo "⚠️  Port 3000 is already in use:"
    lsof -i :3000
    echo "   Stop the service using port 3000 or use a different port"
else
    echo "✅ Port 3000 is available"
fi

# Check existing ATChess containers
echo ""
echo "📦 Checking existing ATChess containers..."
if docker-compose ps 2> /dev/null | grep -q "atchess\|pds"; then
    echo "ℹ️  Found existing containers:"
    docker-compose ps
    echo ""
    echo "🛠️  To reset:"
    echo "   docker-compose down -v"
    echo "   docker-compose up -d"
else
    echo "✅ No existing ATChess containers found"
fi

echo ""
echo "📊 Summary:"
echo "==========="
echo "✓ Docker installation: $(docker --version | cut -d' ' -f3)"
echo "✓ Docker Compose: $(docker-compose --version | cut -d' ' -f3)"
echo "✓ Docker daemon: Running"
echo "✓ Available space: $(df -h $(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo '/var/lib/docker') | tail -1 | awk '{print $4}')"

echo ""
echo "📖 For more help, see: docs/local-pds-setup.md"
echo "🚀 Try running: ./scripts/quick-start.sh"