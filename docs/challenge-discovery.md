# Challenge Discovery in ATChess

## Current Limitation

**ATChess currently lacks a challenge discovery mechanism**. While the system can create challenges, there's no implemented way for users to discover challenges directed at them. This is a critical gap for cross-PDS gameplay.

## How It Currently Works (Incomplete)

1. **Challenge Creation**: User1 creates a challenge via `POST /api/challenges`
   - Challenge is stored in User1's repository as an `app.atchess.challenge` record
   - Contains challenger DID, challenged DID, and game parameters

2. **Missing Discovery**: User2 has no way to:
   - Know the challenge exists
   - List incoming challenges
   - Accept or decline challenges

## How Challenge Discovery Should Work

In the AT Protocol ecosystem, there are several approaches to implement challenge discovery:

### Option 1: Repository Polling (Simple but Inefficient)

User2's client would periodically query known repositories for challenges:

```javascript
// Pseudo-code for challenge discovery
async function discoverChallenges(userDID) {
  const challenges = [];
  
  // Query user's own repository for challenges created by others
  const records = await atproto.listRecords({
    repo: userDID,
    collection: 'app.atchess.challenge',
    filter: { challenged: userDID }
  });
  
  // Would need to query other users' repos too
  // This is the main limitation - knowing which repos to query
  
  return challenges;
}
```

### Option 2: Firehose Subscription (Recommended)

Subscribe to the AT Protocol firehose for real-time updates:

```javascript
// Subscribe to firehose for challenge events
firehose.subscribe({
  collections: ['app.atchess.challenge'],
  filter: (event) => {
    // Check if challenge is for current user
    return event.record.challenged === currentUserDID;
  },
  onEvent: (event) => {
    // Handle new challenge notification
    notifyUser(event.record);
  }
});
```

### Option 3: Indexing Service

A dedicated service that indexes challenges across all PDSes:

1. Service subscribes to firehose
2. Indexes all chess challenges
3. Provides query API for users to find their challenges
4. Similar to how Bluesky AppView indexes posts

### Option 4: Push Notifications

When creating a challenge, also create a notification record:

1. Create challenge in challenger's repo
2. Create notification in challenged user's repo (if permissions allow)
3. Or use AT Protocol's notification system when available

## Temporary Workarounds

Until proper discovery is implemented:

### 1. Manual Challenge Sharing

Users must share challenge URIs out-of-band:
- Copy challenge URI from creation response
- Share via other communication channels
- Recipient manually accepts using the URI

### 2. Same-PDS Testing

For testing, use accounts on the same PDS:
- Easier to query records within same PDS
- Can list all challenges and filter client-side
- Not a real solution for federation

### 3. Known Players List

Maintain a list of known player DIDs:
- Query specific players' repositories
- Works for small, known groups
- Doesn't scale to open federation

## Implementation Requirements

To properly implement challenge discovery, ATChess needs:

1. **Firehose Client**: Subscribe to AT Protocol firehose
2. **Challenge Index**: Local cache of relevant challenges  
3. **Polling Fallback**: For when firehose is unavailable
4. **UI Updates**: Show pending challenges in web interface
5. **Real-time Updates**: WebSocket or SSE for live notifications

## Example Implementation Plan

```go
// 1. Add to AT Protocol client
func (c *Client) ListIncomingChallenges(ctx context.Context) ([]*Challenge, error) {
    // Query own repository for challenge records where challenged = self.did
    // This would work if challengers wrote to challenged user's repo
}

func (c *Client) SubscribeToChallenges(ctx context.Context, callback func(*Challenge)) error {
    // Subscribe to firehose for challenge events
    // Filter for challenges where challenged = self.did
}

// 2. Add to web service
func (s *Service) ListChallengesHandler(w http.ResponseWriter, r *http.Request) {
    // Return list of incoming challenges
    // Could combine multiple discovery methods
}

// 3. Add to web UI
function pollForChallenges() {
    // Periodically check for new challenges
    // Update UI with pending challenges
    // Show accept/decline buttons
}
```

## Federation Implications

The lack of challenge discovery highlights a key challenge in federated systems:

1. **No Central Index**: Unlike centralized systems, no single place to query
2. **Discovery Problem**: How to find relevant data across many repositories
3. **Permission Model**: Can't write to other users' repositories without permission
4. **Real-time Updates**: Need subscription mechanism for timely notifications

## Conclusion

Challenge discovery is essential for ATChess to work as a truly federated chess platform. The current implementation can create challenges but not discover them, making cross-PDS gameplay impossible without manual coordination. Implementing firehose subscription or an indexing service would resolve this limitation.