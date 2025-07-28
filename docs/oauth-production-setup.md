# ATChess OAuth Production Setup

## The Issue

When deploying ATChess to a production domain, you may encounter this error:

```
Unable to obtain client metadata for "https://yourdomain.com/client-metadata.json": 
Validation of "redirect_uris.0" failed with error: URL must use "localhost", "127.0.0.1" or "[::1]" as hostname
```

This occurs because AT Protocol OAuth has restrictions on redirect URIs for unregistered clients.

## Understanding AT Protocol OAuth Client Types

### 1. Development/Localhost Clients
- Can only use localhost, 127.0.0.1, or [::1] as redirect URIs
- Intended for local development only
- No registration required

### 2. Production Clients
- Can use any domain for redirect URIs
- Require registration or whitelisting with Bluesky/AT Protocol team
- Currently in limited rollout

## Current Workarounds

Since AT Protocol OAuth is still in development and production client registration may not be publicly available yet, here are your options:

### Option 1: Use Password Authentication (Recommended for Now)

1. Don't set `SERVER_BASE_URL` in your environment
2. The system will automatically fall back to password authentication
3. Users will need to create an app password in their Bluesky settings

```bash
# In /etc/atchess/protocol.env, comment out or remove:
# SERVER_BASE_URL=https://atchess.abrah.ms
```

### Option 2: Local Development Mode

For testing OAuth locally:

1. Run ATChess on localhost
2. Access it via http://localhost:8080
3. OAuth will work with localhost redirect URIs

### Option 3: Wait for Production OAuth

AT Protocol OAuth for third-party apps is still being rolled out. Monitor:
- [AT Protocol OAuth Spec](https://atproto.com/specs/oauth)
- [Bluesky OAuth Documentation](https://docs.bsky.app/docs/advanced-guides/oauth-client)

## Future Production Setup

Once production OAuth clients are available:

1. Register your client with AT Protocol
2. Your domain will be whitelisted for redirect URIs
3. OAuth will work with your production domain

## Checking OAuth Availability

You can test if your domain is whitelisted by accessing:
```
https://yourdomain.com/client-metadata.json
```

And checking if Bluesky accepts the redirect URIs when attempting login.

## References

- [AT Protocol OAuth Spec](https://atproto.com/specs/oauth)
- [OAuth Client Implementation Guide](https://docs.bsky.app/docs/advanced-guides/oauth-client)
- [GitHub Discussion on OAuth Client Security](https://github.com/bluesky-social/atproto/discussions/3950)