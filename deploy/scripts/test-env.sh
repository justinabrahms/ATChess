#!/bin/bash
# Test environment loading for ATChess

echo "Testing environment variable loading..."
echo ""
echo "Method 1: Direct environment check"
echo "ATPROTO_PDS_URL=$ATPROTO_PDS_URL"
echo "ATPROTO_HANDLE=$ATPROTO_HANDLE"
echo ""

echo "Method 2: Source and check"
set -a
source /etc/atchess/protocol.env
set +a
echo "After sourcing:"
echo "ATPROTO_PDS_URL=$ATPROTO_PDS_URL"
echo "ATPROTO_HANDLE=$ATPROTO_HANDLE"
echo ""

echo "Method 3: Export and check"
export $(grep -v '^#' /etc/atchess/protocol.env | xargs)
echo "After export:"
echo "ATPROTO_PDS_URL=$ATPROTO_PDS_URL"
echo "ATPROTO_HANDLE=$ATPROTO_HANDLE"