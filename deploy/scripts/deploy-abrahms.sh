#!/bin/bash
# Manual deployment script for abrah.ms server
# Use this for deploying without GitHub Actions

set -euo pipefail

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# Configuration
DEPLOY_HOST="${DEPLOY_HOST:-abrah.ms}"
DEPLOY_USER="${DEPLOY_USER:-atchess-deploy}"
DEPLOY_PORT="${DEPLOY_PORT:-22}"
APP_DIR="/srv/atchess/app"

echo -e "${GREEN}ATChess Deployment Script${NC}"
echo "=========================="
echo "Host: $DEPLOY_HOST"
echo "User: $DEPLOY_USER"
echo ""

# Check if binaries exist
if [[ ! -f "bin/atchess-protocol" ]] || [[ ! -f "bin/atchess-web" ]]; then
    echo -e "${YELLOW}Building binaries...${NC}"
    make build
fi

# Create deployment archive
echo -e "${YELLOW}Creating deployment archive...${NC}"
tar -czf deploy.tar.gz \
    bin/atchess-protocol \
    bin/atchess-web \
    web/static

# Deploy to server
echo -e "${YELLOW}Copying files to server...${NC}"
scp -P $DEPLOY_PORT deploy.tar.gz $DEPLOY_USER@$DEPLOY_HOST:/tmp/

echo -e "${YELLOW}Deploying application...${NC}"
ssh -p $DEPLOY_PORT $DEPLOY_USER@$DEPLOY_HOST << 'ENDSSH'
set -e

# Extract files
cd /srv/atchess/app
tar -xzf /tmp/deploy.tar.gz
rm /tmp/deploy.tar.gz

# Move binaries
mv bin/atchess-protocol bin/atchess-web ./
rmdir bin

# Make binaries executable
chmod +x atchess-protocol atchess-web

# Restart services
echo "Restarting services..."
sudo systemctl restart atchess-protocol
sudo systemctl restart atchess-web

# Check status
sleep 2
sudo systemctl status atchess-protocol --no-pager
sudo systemctl status atchess-web --no-pager
ENDSSH

# Clean up local archive
rm -f deploy.tar.gz

echo -e "${GREEN}Deployment complete!${NC}"
echo ""
echo "Checking health endpoint..."
sleep 3
curl -s https://atchess.abrah.ms/api/health | jq . || echo "Health check failed"

echo ""
echo -e "${GREEN}ATChess is available at: https://atchess.abrah.ms${NC}"