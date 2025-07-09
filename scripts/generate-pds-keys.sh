#!/bin/bash

# Generate secure keys for PDS production deployment
# This script creates cryptographically secure keys for PDS operation

echo "🔐 PDS Key Generation for Production"
echo "===================================="
echo ""
echo "⚠️  WARNING: This generates keys for PRODUCTION use."
echo "   Keep these keys secure and never commit them to version control!"
echo ""

# Check if openssl is available
if ! command -v openssl &> /dev/null; then
    echo "❌ OpenSSL is required but not installed."
    echo "   Install OpenSSL and try again."
    exit 1
fi

# Generate a secure PLC rotation key (32 bytes = 64 hex characters)
echo "🔑 Generating PLC rotation key..."
PLC_KEY=$(openssl rand -hex 32)

# Generate a secure JWT secret (32 bytes = 64 hex characters)
echo "🔑 Generating JWT secret..."
JWT_SECRET=$(openssl rand -hex 32)

# Generate a secure service signing key (32 bytes = 64 hex characters)
echo "🔑 Generating service signing key..."
SERVICE_KEY=$(openssl rand -hex 32)

echo ""
echo "✅ Keys generated successfully!"
echo ""
echo "📋 Add these to your production docker-compose.yml:"
echo "=================================================="
echo ""
echo "environment:"
echo "  - PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX=$PLC_KEY"
echo "  - PDS_JWT_SECRET=$JWT_SECRET"
echo "  - PDS_SERVICE_SIGNING_KEY=$SERVICE_KEY"
echo "  # Remove PDS_DEV_MODE=true for production"
echo ""
echo "🔒 Security Notes:"
echo "=================="
echo "• Store these keys in a secure location (password manager, encrypted vault)"
echo "• Never commit these keys to version control"
echo "• Use environment variables or Docker secrets in production"
echo "• Rotate keys periodically for enhanced security"
echo "• Each PDS instance should have unique keys"
echo ""
echo "💾 Save to file? (keys.env)"
read -p "Save keys to keys.env file? (y/N): " -n 1 -r
echo

if [[ $REPLY =~ ^[Yy]$ ]]; then
    cat > keys.env << EOF
# PDS Production Keys - Generated $(date)
# KEEP THESE SECURE - DO NOT COMMIT TO VERSION CONTROL

PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX=$PLC_KEY
PDS_JWT_SECRET=$JWT_SECRET
PDS_SERVICE_SIGNING_KEY=$SERVICE_KEY

# Usage in docker-compose.yml:
# environment:
#   - PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX=\${PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX}
#   - PDS_JWT_SECRET=\${PDS_JWT_SECRET}
#   - PDS_SERVICE_SIGNING_KEY=\${PDS_SERVICE_SIGNING_KEY}

# Load with: source keys.env && docker-compose up -d
EOF
    
    # Make the file readable only by owner
    chmod 600 keys.env
    
    echo "✅ Keys saved to keys.env (readable only by you)"
    echo "📖 Load with: source keys.env && docker-compose up -d"
    
    # Add to gitignore if it exists
    if [ -f .gitignore ]; then
        if ! grep -q "keys.env" .gitignore; then
            echo "keys.env" >> .gitignore
            echo "✅ Added keys.env to .gitignore"
        fi
    fi
fi