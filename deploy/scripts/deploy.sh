#!/bin/bash
set -euo pipefail

# Deployment script for ATChess
# Usage: ./deploy.sh [user@host]

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# Configuration
REMOTE="${1:-}"
REMOTE_DIR="/opt/atchess"
REMOTE_STATIC="/var/www/atchess"

if [ -z "$REMOTE" ]; then
    echo -e "${RED}Usage: $0 user@host${NC}"
    echo "Example: $0 root@atchess.example.com"
    exit 1
fi

echo -e "${GREEN}ATChess Deployment Script${NC}"
echo "========================="
echo "Deploying to: $REMOTE"
echo ""

# Build binaries
echo -e "${YELLOW}Building binaries...${NC}"
make clean
make build

# Create deployment archive
echo -e "${YELLOW}Creating deployment archive...${NC}"
DEPLOY_FILE="atchess-deploy-$(date +%Y%m%d-%H%M%S).tar.gz"
tar -czf "$DEPLOY_FILE" \
    bin/atchess-protocol \
    bin/atchess-web \
    web/static \
    deploy/systemd/*.service \
    deploy/nginx/*.conf

# Upload to server
echo -e "${YELLOW}Uploading to server...${NC}"
scp "$DEPLOY_FILE" "$REMOTE:/tmp/"

# Deploy on server
echo -e "${YELLOW}Deploying on server...${NC}"
ssh "$REMOTE" bash -s << EOF
set -euo pipefail

# Extract files
cd /tmp
tar -xzf "$DEPLOY_FILE"

# Backup current binaries
if [ -f "$REMOTE_DIR/bin/atchess-protocol" ]; then
    sudo cp "$REMOTE_DIR/bin/atchess-protocol" "$REMOTE_DIR/bin/atchess-protocol.backup"
fi
if [ -f "$REMOTE_DIR/bin/atchess-web" ]; then
    sudo cp "$REMOTE_DIR/bin/atchess-web" "$REMOTE_DIR/bin/atchess-web.backup"
fi

# Deploy new binaries
sudo cp bin/atchess-protocol bin/atchess-web "$REMOTE_DIR/bin/"
sudo chown atchess:atchess "$REMOTE_DIR/bin/"*
sudo chmod +x "$REMOTE_DIR/bin/"*

# Deploy static files
sudo cp -r web/static/* "$REMOTE_STATIC/"
sudo chown -R www-data:www-data "$REMOTE_STATIC/"

# Update systemd services if changed
if ! diff -q deploy/systemd/atchess-protocol.service /etc/systemd/system/atchess-protocol.service >/dev/null 2>&1; then
    echo "Updating protocol service file..."
    sudo cp deploy/systemd/atchess-protocol.service /etc/systemd/system/
    sudo systemctl daemon-reload
fi

if ! diff -q deploy/systemd/atchess-web.service /etc/systemd/system/atchess-web.service >/dev/null 2>&1; then
    echo "Updating web service file..."
    sudo cp deploy/systemd/atchess-web.service /etc/systemd/system/
    sudo systemctl daemon-reload
fi

# Update nginx config if changed
if ! diff -q deploy/nginx/atchess.conf /etc/nginx/sites-available/atchess.conf >/dev/null 2>&1; then
    echo "Updating nginx configuration..."
    sudo cp deploy/nginx/atchess.conf /etc/nginx/sites-available/
    sudo nginx -t && sudo systemctl reload nginx
fi

# Restart services
echo "Restarting services..."
sudo systemctl restart atchess-protocol
sudo systemctl restart atchess-web

# Cleanup
rm -rf /tmp/"$DEPLOY_FILE" /tmp/bin /tmp/web /tmp/deploy

# Check status
echo ""
echo "Deployment complete. Service status:"
sudo systemctl status atchess-protocol --no-pager | grep "Active:"
sudo systemctl status atchess-web --no-pager | grep "Active:"
EOF

# Cleanup local file
rm -f "$DEPLOY_FILE"

echo ""
echo -e "${GREEN}Deployment complete!${NC}"
echo ""
echo "Check the application at: https://$REMOTE"
echo "View logs with: ssh $REMOTE 'atchess-logs all'"