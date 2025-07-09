# Discovered Bugs Test Cases

This document tracks bugs discovered during development and testing of ATChess.

## Bug 1: CORS OPTIONS Request Handling

**Issue**: Browser sends CORS preflight OPTIONS requests when making POST requests to `/api/games/{id}/moves`, but server returns errors.

**Root Cause**: CORS middleware wasn't properly handling OPTIONS requests for routes with regex patterns like `{id:.*}`.

**Test Case**:
```bash
curl -X OPTIONS 'http://localhost:8080/api/games/test/moves' \
  -H "Origin: http://localhost:8081" \
  -H "Access-Control-Request-Method: POST" \
  -H "Access-Control-Request-Headers: content-type"
```

**Expected**: 200 OK with proper CORS headers
**Actual**: Error or missing CORS headers

**Fix**: Added explicit OPTIONS handlers for all API routes.

---

## Bug 2: AT Protocol URI Routing Issues

**Issue**: AT Protocol URIs contain special characters (`:`, `/`, `//`) that cause routing problems when used in URL paths.

**Root Cause**: URL encoding of AT Protocol URIs like `at://did:plc:...` causes 301 redirects and routing failures.

**Test Case**:
```bash
curl 'http://localhost:8080/api/games/at%3A%2F%2Fdid%3Aplc%3Astyupz2ghvg7hrq4optipm7s%2Fapp.atchess.game%2F3ltivg2d6bk2e/moves' \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"from":"e2","to":"e4","fen":"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1","game_id":"at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e"}'
```

**Expected**: 200 OK with move result
**Actual**: 301 Moved Permanently redirect

**Fix**: Moved game ID from URL path to request body (`/api/moves` endpoint).

---

## Bug 3: Missing JSON Struct Tags

**Issue**: API responses returning empty or undefined fields in JavaScript.

**Root Cause**: `MoveResult` struct was missing JSON tags, so fields like `FEN` and `SAN` weren't being serialized correctly.

**Test Case**:
```javascript
// After making a move, check the response
const result = await response.json();
console.log(result.fen); // Should not be undefined
console.log(result.san); // Should not be undefined
```

**Expected**: Proper JSON response with lowercase field names
**Actual**: Missing fields or wrong field names

**Fix**: Added JSON tags to `MoveResult` struct.

---

## Bug 4: Empty FEN String Validation

**Issue**: Chess engine throws error "invalid FEN: chess: fen invalid notation must have 6 sections" when empty FEN is passed.

**Root Cause**: JavaScript or API not properly handling FEN strings, passing empty strings to chess engine.

**Test Case**:
```bash
curl -X POST 'http://localhost:8080/api/moves' \
  -H 'Content-Type: application/json' \
  -d '{"from":"e2","to":"e4","fen":"","game_id":"test"}'
```

**Expected**: 400 Bad Request with clear error message
**Actual**: Server error with chess engine validation failure

**Fix**: Added FEN validation and fallback to initial position.

---

## Bug 5: AT Protocol URI Parsing

**Issue**: `GetGame` API call fails because AT Protocol URIs aren't properly parsed to extract repo and rkey.

**Root Cause**: Code was using full URI as `rkey` parameter instead of parsing URI components.

**Test Case**:
```bash
# This should work after fix
curl 'http://localhost:8080/api/games/YXQ6Ly9kaWQ6cGxjOnN0eXVwejJnaHZnN2hycTRvcHRpcG03cy9hcHAuYXRjaGVzcy5nYW1lLzNsdGl3anFvNjIyMmU='
```

**Expected**: 200 OK with game state
**Actual**: 404 Not Found or AT Protocol error

**Fix**: Added proper URI parsing to extract repo DID and record key.

---

## Bug 6: Base64 Padding Truncation

**Issue**: Base64 encoded game IDs being truncated during encoding/decoding process.

**Root Cause**: JavaScript `encodeGameId` function was removing `=` padding characters, causing decoding issues.

**Test Case**:
```javascript
// Test encoding/decoding round trip
const gameId = "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltiwjqo6222e";
const encoded = this.encodeGameId(gameId);
const decoded = this.decodeGameId(encoded);
console.log(decoded === gameId); // Should be true
```

**Expected**: Perfect round-trip encoding/decoding
**Actual**: Truncated game IDs due to missing padding

**Fix**: Preserved padding characters in JavaScript encoding.

---

## Bug 7: Game Creation JSON Serialization

**Issue**: Game creation response showing "undefined" for game ID in UI.

**Root Cause**: `Game` struct fields weren't properly serialized to JSON due to missing or incorrect JSON tags.

**Test Case**:
```bash
curl -X POST 'http://localhost:8080/api/games' \
  -H 'Content-Type: application/json' \
  -d '{"opponent_did":"did:plc:test","color":"white"}'
```

**Expected**: JSON response with properly formatted field names
**Actual**: Missing or incorrectly named fields

**Fix**: Added proper JSON tags to `Game` struct with lowercase field names.

---

## Testing Guidelines

1. **CORS Testing**: Always test with different origins and preflight requests
2. **URI Encoding**: Test with special characters and complex URIs
3. **JSON Serialization**: Verify all struct fields have proper JSON tags
4. **Error Handling**: Test with invalid/empty inputs
5. **Round-trip Testing**: Ensure encoding/decoding works both ways
6. **AT Protocol Integration**: Test with actual PDS and valid DIDs

## Common Test Data

**Test DIDs**:
- Player 1: `did:plc:styupz2ghvg7hrq4optipm7s`
- Player 2: `did:plc:yguha7jixn3rlblla2pzbmwl`

**Test Game URI Format**:
`at://did:plc:USER_ID/app.atchess.game/RECORD_KEY`

**Initial FEN**:
`rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1`