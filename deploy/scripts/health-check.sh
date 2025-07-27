#!/bin/bash
# Health check script for ATChess services

echo "=== ATChess Services Health Check ==="
echo ""

# Check protocol service
echo "Protocol Service (port 8080):"
if curl -s -f http://localhost:8080/api/health > /dev/null 2>&1; then
    echo "✅ Protocol service is healthy"
    curl -s http://localhost:8080/api/health | jq .
else
    echo "❌ Protocol service is not responding"
    echo "   Status: $(sudo systemctl is-active atchess-protocol)"
fi

echo ""

# Check web service  
echo "Web Service (port 8081):"
if curl -s -f http://localhost:8081/health > /dev/null 2>&1; then
    echo "✅ Web service is healthy"
    curl -s http://localhost:8081/health | jq .
else
    echo "❌ Web service is not responding"
    echo "   Status: $(sudo systemctl is-active atchess-web)"
fi

echo ""

# Check Caddy
echo "Caddy Proxy:"
if curl -s -f https://atchess.abrah.ms/api/health > /dev/null 2>&1; then
    echo "✅ Caddy proxy is working"
else
    echo "⚠️  Caddy proxy to protocol service not working"
    echo "   This is expected if protocol service is down"
fi

echo ""
echo "Service Summary:"
sudo systemctl status atchess-protocol --no-pager --lines=3
echo ""
sudo systemctl status atchess-web --no-pager --lines=3