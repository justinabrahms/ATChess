# Local PDS Setup for Development

This guide helps you set up a local AT Protocol Personal Data Server (PDS) for testing ATChess.

## Prerequisites

- Docker and Docker Compose installed
- Go 1.21+ installed
- Make installed

## Quick Setup

### 1. Run Local PDS

The `docker-compose.yml` file is already configured with development keys. For production, you'll need to generate secure keys.

**For Development (using provided docker-compose.yml):**
The included configuration uses development keys that are safe for local testing.

**For Production:**
Generate secure keys with the provided script:

```bash
./scripts/generate-pds-keys.sh
```

This creates cryptographically secure keys for:
- `PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX` - Identity management
- `PDS_JWT_SECRET` - Session token signing  
- `PDS_SERVICE_SIGNING_KEY` - Service authentication

⚠️ **Security Note**: The development keys in docker-compose.yml are for local testing only. Never use them in production!

Start the PDS:

```bash
docker-compose up -d
```

Wait for the PDS to be healthy:

```bash
docker-compose ps
```

### 2. Create Test Accounts

Create a script to set up test accounts:

```bash
# scripts/create-test-accounts.sh
#!/bin/bash

PDS_URL="http://localhost:3000"
ADMIN_PASSWORD="admin"

# Create two test accounts for chess games
echo "Creating test accounts..."

# Player 1
curl -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "player1@chess.test",
    "handle": "player1.localhost",
    "password": "player1pass",
    "inviteCode": ""
  }'

echo ""

# Player 2  
curl -X POST "$PDS_URL/xrpc/com.atproto.server.createAccount" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "player2@chess.test", 
    "handle": "player2.localhost",
    "password": "player2pass",
    "inviteCode": ""
  }'

echo ""
echo "Test accounts created!"
```

Make it executable and run:

```bash
chmod +x scripts/create-test-accounts.sh
./scripts/create-test-accounts.sh
```

### 3. Configure ATChess

Create a `config.yaml` for local development:

```yaml
# config.yaml
server:
  host: localhost
  port: 8080
  
atproto:
  pds_url: http://localhost:3000
  handle: "atchess.localhost"
  password: "atchess-service-password"
  
development:
  debug: true
  log_level: debug
```

### 4. Verify Setup

Test the PDS is working:

```bash
# Check PDS health
curl http://localhost:3000/_health

# List DIDs
curl http://localhost:3000/xrpc/com.atproto.sync.listRepos
```

## Development Workflow

1. **Start PDS**: `docker-compose up -d`
2. **Run ATChess**: `make dev`
3. **View logs**: `docker-compose logs -f pds`
4. **Reset data**: `docker-compose down -v && docker-compose up -d`

## Troubleshooting

### Docker Image Pull Issues

If you see errors like "failed to resolve reference" or "cannot allocate memory":

```bash
# Clean up Docker system
docker system prune -af

# Restart Docker daemon
# On macOS: Restart Docker Desktop
# On Linux: sudo systemctl restart docker

# Try pulling the image manually
docker pull ghcr.io/bluesky-social/pds:latest

# If still failing, try with a specific version
docker pull ghcr.io/bluesky-social/pds:0.4
```

### Memory Issues

If Docker runs out of memory:

```bash
# Check Docker memory limits
docker system df

# Increase Docker Desktop memory allocation (macOS/Windows):
# Docker Desktop → Settings → Resources → Memory → Increase to 4GB+

# On Linux, ensure sufficient swap space:
sudo swapon --show
```

### Alternative Image Pull Methods

If the main registry is unavailable:

```bash
# Try pulling without cache
docker pull --no-cache ghcr.io/bluesky-social/pds:latest

# Or specify a different registry mirror if available
docker pull docker.io/bluesky/pds:latest
```

### PDS Container Issues

```bash
# Check if PDS container is running
docker-compose ps

# View detailed logs
docker-compose logs -f pds

# Restart the PDS
docker-compose restart pds

# Complete reset (removes all data)
docker-compose down -v
docker-compose up -d
```

### Network Connectivity

```bash
# Test if PDS is accessible
curl http://localhost:3000/_health

# Check which process is using port 3000
lsof -i :3000

# Test DNS resolution
nslookup ghcr.io
```

### Account Creation Issues

```bash
# Ensure PDS is fully ready before creating accounts
docker-compose logs pds | grep "server listening"

# Check invite requirements
curl http://localhost:3000/xrpc/com.atproto.server.describeServer

# Manually create an account
curl -X POST http://localhost:3000/xrpc/com.atproto.server.createAccount \
  -H "Content-Type: application/json" \
  -d '{"email": "test@test.com", "handle": "test.localhost", "password": "testpass"}'
```

### Complete Reset

If all else fails, reset everything:

```bash
# Stop and remove all containers and volumes
docker-compose down -v

# Remove all images and system cache
docker system prune -af

# Start fresh
docker-compose up -d

# Wait for PDS to be ready
./scripts/create-test-accounts.sh
```

## Next Steps

With the local PDS running and test accounts created, you can:

1. Test AT Protocol operations using the test accounts
2. Develop chess game features with real federation
3. Test multiplayer games between accounts

## Useful Commands

```bash
# Stop PDS
docker-compose down

# Reset all data
docker-compose down -v

# View PDS logs
docker-compose logs -f pds

# Check account info
curl -X POST http://localhost:3000/xrpc/com.atproto.server.getSession \
  -H "Content-Type: application/json" \
  -d '{"identifier": "player1.localhost", "password": "player1pass"}'
```