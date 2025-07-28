# ATChess Deployment

This directory contains deployment scripts and configurations for ATChess instances.

## Scripts

### `scripts/setup-atchess-abrahms.sh`

Server setup script for the atchess.abrah.ms deployment. This script handles the **server configuration only** - binaries are deployed automatically by GitHub Actions on every push to main.

**What this script does:**
- Creates the `atchess` service user
- Sets up directory structure at `/srv/atchess/app`
- Generates OAuth keys (if they don't exist)
- Configures systemd services
- Sets up log rotation
- Creates environment configuration files

**What this script does NOT do:**
- Does not download or build binaries (handled by auto-deploy)
- Does not clone source code
- Does not require Go compiler

**Usage:**
```bash
# Run from anywhere to set up or update server configuration
wget https://raw.githubusercontent.com/justinabrahms/atchess/main/deploy/scripts/setup-atchess-abrahms.sh
sudo bash ./setup-atchess-abrahms.sh
```

The script is idempotent - safe to run multiple times. It preserves existing OAuth keys and environment configurations.

**Security features:**
- OAuth keys stored in `/etc/atchess/keys/` with read-only permissions
- Service runs as non-root user
- Systemd hardening applied
- Proper file permissions throughout

## Creating Your Own Deployment

To create a deployment script for your own domain:

1. Copy `scripts/setup-atchess-abrahms.sh` to `scripts/setup-atchess-yourdomain.sh`
2. Update the `DOMAIN` variable in the script
3. Adjust any other deployment-specific settings
4. Run with sudo on your server

## Deployment Architecture

ATChess uses a two-part deployment system:

1. **Auto-deployment** (`.github/workflows/deploy-abrahms.yml`):
   - Triggers on every push to main
   - Builds binaries with GitHub Actions
   - Deploys via SSH to `/srv/atchess/app`
   - Restarts services automatically
   - No manual intervention needed

2. **Server setup** (this script):
   - Run manually when needed
   - Sets up systemd, users, directories
   - Manages OAuth keys and environment
   - Safe to run multiple times

## OAuth Key Management

The deployment scripts handle OAuth key generation automatically. Keys are:
- Generated only if they don't exist (preserving existing keys)
- Stored in `/etc/atchess/keys/` with 400 permissions
- Owned by the service user
- Never overwritten on subsequent deployments

See [OAuth Key Setup](../docs/oauth-key-setup.md) for manual key management.