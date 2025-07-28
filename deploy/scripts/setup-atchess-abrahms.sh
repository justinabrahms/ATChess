#!/bin/bash

# ATChess Server Deployment Script for atchess.abrah.ms
# This script handles server deployment including OAuth key management

set -euo pipefail

# Configuration for atchess.abrah.ms deployment
# Match the paths used by the auto-deploy workflow
ATCHESS_DIR="/srv/atchess/app"
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

# Create deploy user if doesn't exist (for GitHub Actions)
DEPLOY_USER="atchess-deploy"
if ! id -u "$DEPLOY_USER" >/dev/null 2>&1; then
    echo "👤 Creating deploy user..."
    useradd -r -s /bin/bash -d /home/atchess-deploy -m "$DEPLOY_USER"
fi

# Add deploy user to atchess group for file access
usermod -a -G "$ATCHESS_USER" "$DEPLOY_USER"

# Set up minimal sudoers for deploy user (only for systemctl)
echo "🔐 Configuring deploy user permissions..."
cat > /etc/sudoers.d/atchess-deploy <<EOF
# Allow atchess-deploy to manage ATChess services only
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl stop atchess-protocol
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl stop atchess-web
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl restart atchess-protocol
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl restart atchess-web
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl daemon-reload
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl status atchess-protocol
$DEPLOY_USER ALL=(root) NOPASSWD: /bin/systemctl status atchess-web
EOF
chmod 440 /etc/sudoers.d/atchess-deploy

# Create directory structure
echo "📁 Setting up directories..."
mkdir -p "$ATCHESS_DIR"
mkdir -p "/etc/atchess"
mkdir -p "$KEY_DIR"
mkdir -p "/var/log/atchess"

# Set proper permissions on key directory
chown root:"$ATCHESS_USER" "$KEY_DIR"
chmod 750 "$KEY_DIR"

# Note: This setup script assumes your auto-deploy workflow handles binary deployment
# We only need to ensure directories exist and download the key generator if needed

# Create required directories
echo "📁 Setting up directory structure..."
mkdir -p "$ATCHESS_DIR/bin"
mkdir -p "$ATCHESS_DIR/web/static"
mkdir -p "$ATCHESS_DIR/lexicons"
mkdir -p "/srv/atchess/staging"

# OAuth key generator is not needed on the server - generate keys locally

# Ensure ownership and permissions for deployment
# Set group ownership to allow deploy user to write
chown -R "$ATCHESS_USER:$ATCHESS_USER" "$ATCHESS_DIR"
# Add group write permissions for deployment
chmod -R g+w "$ATCHESS_DIR"
# Ensure directories have group execute permission
find "$ATCHESS_DIR" -type d -exec chmod g+x {} \;

# Set up staging directory permissions
chown "$ATCHESS_USER:$ATCHESS_USER" /srv/atchess/staging
chmod 775 /srv/atchess/staging

echo "✅ Directory structure ready for auto-deployment"

# Handle OAuth key
echo "🔐 Checking OAuth authentication..."
if [ ! -f "$OAUTH_KEY_PATH" ]; then
    echo "⚠️  No OAuth private key found at $OAUTH_KEY_PATH"
    echo ""
    echo "To generate an OAuth key:"
    echo "1. On your LOCAL machine, run:"
    echo "   go run github.com/justinabrahms/atchess/cmd/generate-oauth-keys@main"
    echo ""
    echo "2. Copy the private key (between -----BEGIN EC PRIVATE KEY----- and -----END EC PRIVATE KEY-----)"
    echo ""
    echo "3. On this server, create the key file:"
    echo "   sudo mkdir -p $KEY_DIR"
    echo "   sudo chown root:$ATCHESS_USER $KEY_DIR"
    echo "   sudo chmod 750 $KEY_DIR"
    echo "   sudo nano $OAUTH_KEY_PATH"
    echo "   (paste the private key and save)"
    echo "   sudo chown $ATCHESS_USER:$ATCHESS_USER $OAUTH_KEY_PATH"
    echo "   sudo chmod 400 $OAUTH_KEY_PATH"
    echo ""
    echo "4. Run this setup script again"
    echo ""
    echo "❌ Exiting - OAuth key required"
    exit 1
else
    echo "✅ OAuth key exists at $OAUTH_KEY_PATH"
    # Ensure permissions are correct
    chown "$ATCHESS_USER:$ATCHESS_USER" "$OAUTH_KEY_PATH"
    chmod 400 "$OAUTH_KEY_PATH"
fi

# Create environment files (matching existing deployment structure)
echo "📝 Creating environment configuration..."

# Create protocol.env for protocol service
PROTOCOL_ENV="/etc/atchess/protocol.env"
if [ -f "$PROTOCOL_ENV" ]; then
    echo "📋 Preserving existing protocol environment configuration..."
    # Load existing values
    source "$PROTOCOL_ENV" 2>/dev/null || true
fi

# Only create/update if it doesn't exist or is missing required vars
if [ ! -f "$PROTOCOL_ENV" ] || ! grep -q "SERVER_BASE_URL" "$PROTOCOL_ENV" 2>/dev/null; then
    cat > "$PROTOCOL_ENV" <<EOF
# ATChess Protocol Service Environment Configuration
OAUTH_PRIVATE_KEY_PATH=$OAUTH_KEY_PATH
ATPROTO_PDS_URL=${ATPROTO_PDS_URL:-https://bsky.social}
ATPROTO_HANDLE=${ATPROTO_HANDLE:-}
ATPROTO_PASSWORD=${ATPROTO_PASSWORD:-}
ATPROTO_USE_DPOP=true
SERVER_BASE_URL=https://$DOMAIN
SERVER_HOST=0.0.0.0
SERVER_PORT=8080
EOF
    chmod 644 "$PROTOCOL_ENV"
    echo "✅ Created/updated protocol.env"
else
    echo "✅ protocol.env already exists with required configuration"
fi

# Create web.env for web service (if needed)
WEB_ENV="/etc/atchess/web.env"
if [ ! -f "$WEB_ENV" ]; then
    cat > "$WEB_ENV" <<EOF
# ATChess Web Service Environment Configuration
SERVER_HOST=0.0.0.0
SERVER_PORT=8081
EOF
    chmod 644 "$WEB_ENV"
fi

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
Environment="ATCHESS_STATIC_DIR=$ATCHESS_DIR/web/static"
Environment="ATCHESS_LEXICONS_DIR=$ATCHESS_DIR/lexicons"
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/protocol.log
StandardError=append:/var/log/atchess/protocol-error.log
EnvironmentFile=/etc/atchess/protocol.env

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
Environment="ATCHESS_STATIC_DIR=$ATCHESS_DIR/web/static"
Restart=always
RestartSec=10
StandardOutput=append:/var/log/atchess/web.log
StandardError=append:/var/log/atchess/web-error.log
EnvironmentFile=/etc/atchess/web.env

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

# Only restart services if binaries exist
if [ -f "$ATCHESS_DIR/bin/atchess-protocol" ] && [ -f "$ATCHESS_DIR/bin/atchess-web" ]; then
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
else
    echo "⚠️  Binaries not found - skipping service restart"
    echo "   Services will start automatically after first deployment"
fi

# Test endpoints only if services are running
if [ -f "$ATCHESS_DIR/bin/atchess-protocol" ] && [ -f "$ATCHESS_DIR/bin/atchess-web" ]; then
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