# Local PDS Setup for Development

This guide helps you set up a local AT Protocol Personal Data Server (PDS) for testing ATChess.

## Prerequisites

- Docker and Docker Compose installed
- Go 1.21+ installed
- Make installed

## Quick Setup

### 1. Run Local PDS

Create a `docker-compose.yml` file in the project root:

```yaml
version: '3.8'

services:
  pds:
    image: ghcr.io/bluesky-social/pds:latest
    ports:
      - "3000:3000"
    environment:
      - PDS_HOSTNAME=localhost:3000
      - PDS_JWT_SECRET=your-secret-key-here
      - PDS_ADMIN_PASSWORD=admin
      - PDS_INVITE_REQUIRED=false
      - PDS_EMAIL_SMTP_URL=smtp://fake
      - PDS_EMAIL_FROM_ADDRESS=noreply@localhost
      - PDS_DATA_DIRECTORY=/pds
      - PDS_BLOBSTORE_DISK_LOCATION=/pds/blocks
      - PDS_DID_PLC_URL=https://plc.directory
      - PDS_BSKY_APP_VIEW_URL=https://api.bsky.app
      - PDS_BSKY_APP_VIEW_DID=did:web:api.bsky.app
      - PDS_CRAWLERS=https://bsky.network
      - LOG_ENABLED=true
    volumes:
      - pds-data:/pds
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/_health"]
      interval: 10s
      timeout: 5s
      retries: 5

volumes:
  pds-data:
```

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

### PDS won't start
- Check ports aren't already in use: `lsof -i :3000`
- Check Docker logs: `docker-compose logs pds`

### Can't create accounts
- Ensure `PDS_INVITE_REQUIRED=false` is set
- Check the PDS is fully started (health check passing)

### Connection errors
- Verify PDS URL in config.yaml matches Docker setup
- Check firewall isn't blocking port 3000

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