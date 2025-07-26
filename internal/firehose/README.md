# AT Protocol Firehose Client for ATChess

This package provides a WebSocket client for connecting to the AT Protocol firehose and filtering chess-related events.

## Features

- Connects to the AT Protocol firehose WebSocket endpoint
- Filters and processes only `app.atchess.*` records
- Handles CAR file parsing and CBOR decoding
- Automatic reconnection with exponential backoff
- Graceful error handling and recovery
- Testable design with mock WebSocket support

## Usage

```go
import (
    "github.com/justinabrahms/atchess/internal/firehose"
    "github.com/rs/zerolog"
)

// Create event handler
handler := func(event firehose.Event) error {
    switch event.Type {
    case firehose.EventTypeMove:
        // Process chess move
    case firehose.EventTypeChallenge:
        // Process new challenge
    case firehose.EventTypeDrawOffer:
        // Process draw offer
    // ... handle other event types
    }
    return nil
}

// Create and start client
client := firehose.NewClient(
    handler,
    firehose.WithLogger(logger),
    // Optional: custom firehose URL
    // firehose.WithURL("wss://custom.firehose.example/..."),
)

if err := client.Start(); err != nil {
    log.Fatal(err)
}

// Stop when done
defer client.Stop()
```

## Event Types

The client recognizes the following chess event types:

- `EventTypeMove` - Chess moves (`app.atchess.move`)
- `EventTypeChallenge` - Game challenges (`app.atchess.challenge`)
- `EventTypeChallengeAcceptance` - Challenge acceptances (`app.atchess.challengeAcceptance`)
- `EventTypeDrawOffer` - Draw offers (`app.atchess.drawOffer`)
- `EventTypeResignation` - Game resignations (`app.atchess.resignation`)
- `EventTypeGame` - Game state updates (`app.atchess.game`)

## Configuration Options

- `WithURL(url)` - Set a custom firehose URL (default: Bluesky's public firehose)
- `WithLogger(logger)` - Set a custom zerolog logger
- `WithInitialReconnectDelay(delay)` - Set initial reconnection delay (default: 1s)

## Reconnection Logic

The client automatically reconnects on connection failure with:
- Initial delay: 1 second (configurable)
- Exponential backoff with factor 2
- Maximum delay: 5 minutes
- Resumes from last sequence number

## Testing

The package includes comprehensive tests with mock WebSocket support:

```go
go test ./internal/firehose/...
```

## Implementation Notes

### Message Format

The firehose sends messages in a specific format:
1. 4-byte header length prefix
2. JSON header containing operation type and metadata
3. CAR (Content Addressable aRchive) data with CBOR-encoded records

### Current Limitations

- The full AT Protocol firehose message parsing is simplified for testing
- CAR file extraction is implemented but not fully tested with real firehose data
- Event records are returned as generic `interface{}` types

### Future Improvements

1. Complete AT Protocol firehose message parsing
2. Type-safe record structures for each event type
3. Metrics and monitoring integration
4. Rate limiting and backpressure handling
5. Persistent sequence tracking for resumption after restarts