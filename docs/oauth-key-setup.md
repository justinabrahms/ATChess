# OAuth Private Key Setup

## Security Notice

**NEVER commit private keys to version control!** The OAuth private key contains sensitive cryptographic material that must be kept secure.

## Important: Dynamic Client Metadata

As of the latest version, ATChess serves OAuth client metadata dynamically from `/client-metadata.json`. This means:
- The public key is automatically included from your configured private key
- No need to manually update any JSON files
- Client metadata always matches your private key
- Works correctly across different domains without hardcoded URLs

## Quick Start (On Your Server)

```bash
# 1. If the private key doesn't exist, generate one
KEY_DIR="/etc/atchess/keys"
KEY_PATH="$KEY_DIR/oauth-private-key.pem"

if [ ! -f "$KEY_PATH" ]; then
    cd /path/to/atchess
    
    # Ensure key directory exists with proper permissions
    sudo mkdir -p "$KEY_DIR"
    sudo chown root:atchess "$KEY_DIR"
    sudo chmod 750 "$KEY_DIR"
    
    # Generate key
    go run cmd/generate-oauth-keys/main.go > oauth-setup.txt
    
    # Extract private key with secure permissions
    (umask 077 && sudo sed -n '/-----BEGIN EC PRIVATE KEY-----/,/-----END EC PRIVATE KEY-----/p' oauth-setup.txt > "$KEY_PATH")
    
    # Set ownership and read-only permissions
    sudo chown atchess:atchess "$KEY_PATH"
    sudo chmod 400 "$KEY_PATH"
    
    echo "New OAuth key generated and saved to $KEY_PATH"
else
    echo "OAuth key already exists at $KEY_PATH"
fi

# 2. Set the key location (add to your service environment or systemd unit)
export OAUTH_PRIVATE_KEY_PATH="/etc/atchess/keys/oauth-private-key.pem"

# 3. Restart the protocol service
sudo systemctl restart atchess-protocol
```

## Detailed Instructions

### 1. Generate a Key Pair

On your server, run:

```bash
cd /path/to/atchess
go run cmd/generate-oauth-keys/main.go
```

This will output:
- A private key (PEM format) - **KEEP THIS SECRET**
- A public key (JWK format) - Automatically served at `/client-metadata.json`

### 2. Configure the Private Key

Since client metadata is now served dynamically, you only need to configure the private key.

Choose one of these methods:

#### Option A: Environment Variable (Recommended for production)

```bash
export OAUTH_PRIVATE_KEY="-----BEGIN EC PRIVATE KEY-----
MHcCAQEE...
-----END EC PRIVATE KEY-----"
```

#### Option B: File (Good for development)

Save the private key to a secure location:
```bash
# Create secure directory if it doesn't exist
sudo mkdir -p /etc/atchess/keys
sudo chown root:atchess /etc/atchess/keys
sudo chmod 750 /etc/atchess/keys

# Save key with secure permissions
(umask 077 && sudo tee /etc/atchess/keys/oauth-private-key.pem > /dev/null << 'EOF'
-----BEGIN EC PRIVATE KEY-----
MHcCAQEE...
-----END EC PRIVATE KEY-----
EOF
)

# Set ownership and read-only permissions
sudo chown atchess:atchess /etc/atchess/keys/oauth-private-key.pem
sudo chmod 400 /etc/atchess/keys/oauth-private-key.pem
```

#### Option C: Custom Path

```bash
export OAUTH_PRIVATE_KEY_PATH="/secure/location/my-key.pem"
```

### 3. Deployment

For production deployments:

1. Store the private key in a secure secret management system
2. Set the `OAUTH_PRIVATE_KEY` environment variable in your deployment
3. The public key is automatically served from your private key - no manual sync needed
4. Never expose the private key in logs or error messages

### 4. Systemd Service Configuration

If using systemd, add the key path to your service unit file:

```ini
[Service]
Environment="OAUTH_PRIVATE_KEY_PATH=/etc/atchess/keys/oauth-private-key.pem"
# OR use EnvironmentFile for better security
EnvironmentFile=/etc/atchess/environment

# Security hardening for key access
ReadOnlyPaths=/etc/atchess/keys
```

## Key Rotation

To rotate keys:

1. Generate a new key pair using `cmd/generate-oauth-keys/main.go`
2. Replace the private key file (keep a backup of the old one)
3. Restart the service - the new public key is automatically served
4. AT Protocol servers will pick up the new metadata from `/client-metadata.json`
5. Keep the old key available for a transition period if needed

## Troubleshooting

### "Failed to load private key"
- Check that the private key file exists and has correct permissions
- Verify the `OAUTH_PRIVATE_KEY` or `OAUTH_PRIVATE_KEY_PATH` is set correctly

### "Token exchange failed" 
- Check that `/client-metadata.json` is accessible and returns your public key
- Verify the key format is correct (EC PRIVATE KEY, not RSA)
- Check server logs for detailed error messages
- Ensure the `OAUTH_PRIVATE_KEY_PATH` or `OAUTH_PRIVATE_KEY` environment variable is set

### "Client metadata not found"
- Verify the service is running and `/client-metadata.json` is accessible
- Check that no static `client-metadata.json` file exists in `web/static/`
- Ensure the OAuth client was properly initialized (check logs)