#!/bin/bash
set -euo pipefail

# Script to update configuration on existing ATChess deployments
# Usage: ./update-config.sh

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

CONFIG_DIR="/etc/atchess"

echo -e "${GREEN}ATChess Configuration Update${NC}"
echo "============================"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}This script must be run as root${NC}" 
   exit 1
fi

# Backup existing configs
if [ -d "$CONFIG_DIR" ]; then
    echo -e "${YELLOW}Backing up existing configuration...${NC}"
    cp -r $CONFIG_DIR ${CONFIG_DIR}.backup.$(date +%Y%m%d-%H%M%S)
fi

# Check if configs exist
if [ ! -f "$CONFIG_DIR/protocol.env" ]; then
    echo -e "${RED}No existing protocol.env found. Creating new one...${NC}"
    mkdir -p $CONFIG_DIR
    
    cat > $CONFIG_DIR/protocol.env <<'EOF'
# ATChess Protocol Service Configuration

# AT Protocol Configuration
ATCHESS_ATPROTO_PDS_URL=https://bsky.social
ATCHESS_ATPROTO_HANDLE=your-handle.bsky.social
ATCHESS_ATPROTO_PASSWORD=your-app-password

# Server Configuration
ATCHESS_SERVER_HOST=0.0.0.0
ATCHESS_SERVER_PORT=8080

# Development Configuration
ATCHESS_DEVELOPMENT_DEBUG=false
ATCHESS_DEVELOPMENT_LOG_LEVEL=info

# Firehose Configuration
ATCHESS_FIREHOSE_ENABLED=false
ATCHESS_FIREHOSE_URL=wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos
EOF
fi

if [ ! -f "$CONFIG_DIR/web.env" ]; then
    echo -e "${RED}No existing web.env found. Creating new one...${NC}"
    
    cat > $CONFIG_DIR/web.env <<'EOF'
# ATChess Web Service Configuration

# Server Configuration
ATCHESS_SERVER_HOST=0.0.0.0
ATCHESS_SERVER_PORT=8081

# Protocol Service URL (internal)
ATCHESS_PROTOCOL_URL=http://localhost:8080

# Public URL (for OAuth callbacks)
ATCHESS_PUBLIC_URL=https://atchess.example.com

# Session Configuration
ATCHESS_SESSION_SECRET=change-this-to-a-secure-random-string
ATCHESS_SESSION_SECURE=true

# Development Configuration
ATCHESS_DEVELOPMENT_DEBUG=false
ATCHESS_DEVELOPMENT_LOG_LEVEL=info
EOF
fi

# Set proper permissions
chmod 600 $CONFIG_DIR/*.env
chown atchess:atchess $CONFIG_DIR/*.env

echo ""
echo -e "${GREEN}Configuration files updated!${NC}"
echo ""
echo "Please edit the following files with your actual values:"
echo "  - $CONFIG_DIR/protocol.env"
echo "  - $CONFIG_DIR/web.env"
echo ""
echo "Then restart the services:"
echo "  sudo systemctl restart atchess-protocol"
echo "  sudo systemctl restart atchess-web"