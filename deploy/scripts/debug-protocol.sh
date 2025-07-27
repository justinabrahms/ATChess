#!/bin/bash
# Debug script for ATChess protocol service

echo "=== ATChess Protocol Service Debug ==="
echo ""

echo "1. Checking service status:"
sudo systemctl status atchess-protocol --no-pager

echo ""
echo "2. Recent logs:"
sudo journalctl -u atchess-protocol -n 50 --no-pager

echo ""
echo "3. Environment configuration:"
echo "File: /etc/atchess/protocol.env"
sudo cat /etc/atchess/protocol.env | grep -v PASSWORD | grep -v password

echo ""
echo "4. Binary exists and is executable:"
ls -la /srv/atchess/app/atchess-protocol

echo ""
echo "5. Testing binary directly:"
cd /srv/atchess/app
echo "Setting environment..."
set -a
source /etc/atchess/protocol.env
set +a
echo "Running binary with environment..."
timeout 5 ./atchess-protocol || echo "Binary exited with code: $?"

echo ""
echo "6. Checking file permissions:"
ls -la /srv/atchess/
ls -la /srv/atchess/logs/
ls -la /etc/atchess/

echo ""
echo "7. Testing AT Protocol connectivity:"
echo "PDS URL from env: $ATPROTO_PDS_URL"
curl -s "$ATPROTO_PDS_URL/xrpc/_health" | jq . || echo "Failed to connect to PDS"