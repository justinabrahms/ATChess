#!/bin/bash

# ATChess Server Deployment Script for atchess.abrah.ms
# This script handles server deployment including OAuth key management

set -euo pipefail

# Configuration for atchess.abrah.ms deployment
ATCHESS_DIR="/opt/atchess"
ATCHESS_USER="atchess"
KEY_DIR="/etc/atchess/keys"
OAUTH_KEY_PATH="$KEY_DIR/oauth-private-key.pem"
SERVICE_NAME="atchess-protocol"
DOMAIN="atchess.abrah.ms"

echo "🚀 ATChess Server Deployment for $DOMAIN"
echo "========================================"

# Check if running as root/sudo
if [[ $EUID -ne 0 ]]; then 
    echo "❌ This script must be run as root"
    exit 1
fi

# Create service user if doesn't exist
if ! id -u "$ATCHESS_USER" >/dev/null 2>&1; then
    echo "👤 Creating service user..."
    useradd -r -s /bin/false -d /var/lib/atchess -m "$ATCHESS_USER"
fi

# Create directory structure
echo "📁 Setting up directories..."
mkdir -p "$ATCHESS_DIR"
mkdir -p "/etc/atchess"
mkdir -p "$KEY_DIR"
mkdir -p "/var/log/atchess"

# Set proper permissions on key directory
chown root:"$ATCHESS_USER" "$KEY_DIR"
chmod 750 "$KEY_DIR"

# Copy application files
echo "📦 Copying application files..."
cp -r cmd internal lexicons web scripts Makefile go.mod go.sum "$ATCHESS_DIR/"
chown -R "$ATCHESS_USER:$ATCHESS_USER" "$ATCHESS_DIR"

# Build the application
echo "🔨 Building ATChess..."
cd "$ATCHESS_DIR"
sudo -u "$ATCHESS_USER" make build

# Handle OAuth key
echo "🔐 Setting up OAuth authentication..."
if [ ! -f "$OAUTH_KEY_PATH" ]; then
    echo "⚠️  No OAuth private key found. Generating new key pair..."
    
    # Build key generator as the service user
    cd "$ATCHESS_DIR"
    sudo -u "$ATCHESS_USER" go build -o generate-oauth-keys cmd/generate-oauth-keys/main.go
    
    # Generate key with restricted umask for security
    TEMP_KEY=$(mktemp)
    sudo -u "$ATCHESS_USER" ./generate-oauth-keys > "$TEMP_KEY"
    
    # Extract private key with proper permissions from the start
    (umask 077 && sed -n '/-----BEGIN EC PRIVATE KEY-----/,/-----END EC PRIVATE KEY-----/p' "$TEMP_KEY" > "$OAUTH_KEY_PATH")
    
    # Set ownership and read-only permissions
    chown "$ATCHESS_USER:$ATCHESS_USER" "$OAUTH_KEY_PATH"
    chmod 400 "$OAUTH_KEY_PATH"
    
    # Clean up
    rm -f "$TEMP_KEY" generate-oauth-keys
    
    echo "✅ New OAuth key generated and saved with secure permissions"
else
    echo "✅ OAuth key already exists, preserving it"
    # Ensure permissions are correct even for existing keys
    chown "$ATCHESS_USER:$ATCHESS_USER" "$OAUTH_KEY_PATH"
    chmod 400 "$OAUTH_KEY_PATH"
fi

# Create environment file
echo "📝 Creating environment configuration..."

# Check if environment file exists and preserve existing values
if [ -f /etc/atchess/environment ]; then
    echo "📋 Preserving existing environment configuration..."
    source /etc/atchess/environment
fi

cat > /etc/atchess/environment <<EOF
# ATChess Environment Configuration
OAUTH_PRIVATE_KEY_PATH=$OAUTH_KEY_PATH
ATPROTO_PDS_URL=${ATPROTO_PDS_URL:-https://bsky.social}
ATPROTO_HANDLE=${ATPROTO_HANDLE:-}
ATPROTO_PASSWORD=${ATPROTO_PASSWORD:-}
ATPROTO_USE_DPOP=true
SERVER_BASE_URL=https://$DOMAIN
SERVER_HOST=0.0.0.0
SERVER_PORT=8080
EOF

# Set secure permissions on environment file
chmod 644 /etc/atchess/environment

# Create systemd service file
echo "🔧 Creating systemd service..."
cat > /etc/systemd/system/$SERVICE_NAME.service <<EOF
[Unit]
Description=ATChess Protocol Service
After=network.target

[Service]
Type=simple
User=$ATCHESS_USER
Group=$ATCHESS_USER
WorkingDirectory=$ATCHESS_DIR
ExecStart=$ATCHESS_DIR/bin/atchess-protocol
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/protocol.log
StandardError=append:/var/log/atchess/protocol-error.log
EnvironmentFile=/etc/atchess/environment

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/atchess
ReadOnlyPaths=/etc/atchess/keys

[Install]
WantedBy=multi-user.target
EOF

# Create web service file
cat > /etc/systemd/system/atchess-web.service <<EOF
[Unit]
Description=ATChess Web Service
After=network.target

[Service]
Type=simple
User=$ATCHESS_USER
Group=$ATCHESS_USER
WorkingDirectory=$ATCHESS_DIR
ExecStart=$ATCHESS_DIR/bin/atchess-web
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/web.log
StandardError=append:/var/log/atchess/web-error.log

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/atchess
ReadOnlyPaths=/etc/atchess/keys

[Install]
WantedBy=multi-user.target
EOF

# Set up log rotation
echo "📋 Setting up log rotation..."
cat > /etc/logrotate.d/atchess <<EOF
/var/log/atchess/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 0640 $ATCHESS_USER $ATCHESS_USER
    sharedscripts
    postrotate
        systemctl reload $SERVICE_NAME >/dev/null 2>&1 || true
        systemctl reload atchess-web >/dev/null 2>&1 || true
    endscript
}
EOF

# Set permissions
chown -R "$ATCHESS_USER:$ATCHESS_USER" /var/log/atchess

# Reload systemd
echo "🔄 Reloading systemd configuration..."
systemctl daemon-reload

# Enable services if not already enabled
systemctl enable $SERVICE_NAME atchess-web 2>/dev/null || true

# Restart services
echo "🚀 Restarting services..."
systemctl restart $SERVICE_NAME atchess-web

# Wait for services to start
sleep 3

# Check service status
if systemctl is-active --quiet $SERVICE_NAME; then
    echo "✅ Protocol service is running"
else
    echo "❌ Protocol service failed to start"
    journalctl -u $SERVICE_NAME -n 50
    exit 1
fi

if systemctl is-active --quiet atchess-web; then
    echo "✅ Web service is running"
else
    echo "❌ Web service failed to start"
    journalctl -u atchess-web -n 50
    exit 1
fi

# Test endpoints
echo "🧪 Testing endpoints..."
if curl -f http://localhost:8080/api/health &> /dev/null; then
    echo "✅ Protocol API is responding"
else
    echo "❌ Protocol API is not responding"
fi

if curl -f http://localhost:8081 &> /dev/null; then
    echo "✅ Web interface is responding"
else
    echo "❌ Web interface is not responding"
fi

if curl -f http://localhost:8080/client-metadata.json &> /dev/null; then
    echo "✅ OAuth client metadata is accessible"
else
    echo "❌ OAuth client metadata is not accessible"
fi

echo ""
echo "🎉 Deployment complete!"
echo "====================="
echo ""
echo "📍 Services running:"
echo "   - Protocol API: http://localhost:8080"
echo "   - Web Interface: http://localhost:8081"
echo "   - OAuth Metadata: http://localhost:8080/client-metadata.json"
echo ""
echo "🔐 OAuth private key: $OAUTH_KEY_PATH"
echo "📝 Environment config: /etc/atchess/environment"
echo "📋 Logs: /var/log/atchess/"
echo ""
echo "💡 Management commands:"
echo "   - View logs: journalctl -u $SERVICE_NAME -f"
echo "   - Restart: systemctl restart $SERVICE_NAME"
echo "   - Stop: systemctl stop $SERVICE_NAME atchess-web"
echo ""

# Check for missing required environment variables
MISSING_VARS=()
if [ -z "${ATPROTO_HANDLE:-}" ]; then
    MISSING_VARS+=("ATPROTO_HANDLE")
fi
if [ -z "${ATPROTO_PASSWORD:-}" ]; then
    MISSING_VARS+=("ATPROTO_PASSWORD")
fi

if [ ${#MISSING_VARS[@]} -gt 0 ]; then
    echo "⚠️  IMPORTANT: The following environment variables need to be configured in /etc/atchess/environment:"
    for var in "${MISSING_VARS[@]}"; do
        echo "   - $var"
    done
    echo ""
    echo "   Edit the file: sudo nano /etc/atchess/environment"
    echo "   Then restart: sudo systemctl restart $SERVICE_NAME"
else
    echo "✅ All required environment variables are configured"
fi