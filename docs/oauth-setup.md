# ATChess OAuth Setup

## Overview

ATChess uses AT Protocol OAuth for authentication, providing a secure "Login with Bluesky" experience without requiring users to share their passwords.

## Configuration

### 1. Set Base URL

The OAuth implementation requires a base URL to be configured for your deployment:

```bash
export SERVER_BASE_URL="https://atchess.abrah.ms"
```

Or in `config.yaml`:

```yaml
server:
  base_url: "https://atchess.abrah.ms"
```

### 2. Client Metadata

The OAuth client metadata is served at `/client-metadata.json` and includes:
- Client ID (the metadata URL itself)
- Redirect URI for OAuth callbacks
- Supported scopes and grant types
- Public key for client authentication (JWK)

### 3. OAuth Flow

1. User enters their Bluesky handle
2. ATChess initiates OAuth authorization with their PDS
3. User is redirected to authorize ATChess
4. After authorization, user is redirected back to `/callback`
5. ATChess exchanges the authorization code for tokens
6. Session is established and user can play chess

## Security Considerations

- OAuth sessions are stored in-memory (not persistent across restarts)
- DPoP (Demonstrating Proof of Possession) is used for enhanced security
- Client authentication uses ES256 (ECDSA with P-256 and SHA-256)
- Sessions expire based on token lifetime from the authorization server

## Deployment Notes

1. **HTTPS Required**: OAuth requires HTTPS in production
2. **Public Metadata**: The client metadata must be publicly accessible
3. **Callback URL**: The callback URL must match exactly what's in the metadata

## Troubleshooting

### "Failed to start authentication"
- Check that the base URL is correctly configured
- Ensure the protocol service is running
- Verify the user's handle is correct

### "Invalid or expired authorization"
- OAuth authorization requests expire after 15 minutes
- User needs to restart the login process

### Session Issues
- Sessions are not persistent across server restarts
- Clear browser localStorage if experiencing issues

## Reverting to Password Authentication

If needed, you can revert to password-based authentication by:
1. Not setting the `SERVER_BASE_URL` configuration
2. The system will fall back to password authentication automatically