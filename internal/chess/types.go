package chess

type GameStatus string

const (
	StatusActive    GameStatus = "active"
	StatusDraw      GameStatus = "draw"
	StatusWhiteWon  GameStatus = "white_won"
	StatusBlackWon  GameStatus = "black_won"
	StatusAbandoned GameStatus = "abandoned"
)

type MoveResult struct {
	From      string `json:"from"`
	To        string `json:"to"`
	SAN       string `json:"san"`
	FEN       string `json:"fen"`
	Check     bool   `json:"check"`
	Checkmate bool   `json:"checkmate"`
	Draw      bool   `json:"draw"`
	GameOver  bool   `json:"gameOver"`
	Result    string `json:"result"`
}

type Game struct {
	ID          string       `json:"id"`
	White       string       `json:"white"` // DID
	Black       string       `json:"black"` // DID
	Status      GameStatus   `json:"status"`
	FEN         string       `json:"fen"`
	PGN         string       `json:"pgn"`
	TimeControl *TimeControl `json:"timeControl"`
	CreatedAt   string       `json:"createdAt"`

	// DerivationIncomplete is true when Status (and, transitively, FEN --
	// see GetGame's doc comment) could not be fully verified because at
	// least one player's repo could not be read while scanning for terminal
	// events. Status in that case is UNPROVEN, not merely "possibly stale":
	// a truncated scan cannot distinguish "this game is still active" from
	// "a terminal event exists but this scan could not see it". It must
	// never be used to authorize a write (e.g. allowing a move, resignation,
	// draw response, or time-violation claim) -- callers making that
	// decision must instead treat the accompanying error (which wraps
	// atproto.ErrIncompleteDerivation) as a hard failure. It may only be
	// used by a caller that has deliberately opted in to rendering a
	// best-effort/degraded read view; no such caller exists yet (atchess-1c9.51).
	DerivationIncomplete bool `json:"derivationIncomplete,omitempty"`
}

type TimeControl struct {
	Type        string `json:"type"`        // "correspondence", "rapid", "blitz"
	DaysPerMove int    `json:"daysPerMove"` // For correspondence games
	Initial     int    `json:"initial"`     // seconds
	Increment   int    `json:"increment"`   // seconds per move
}

type Challenge struct {
	ID              string
	Challenger      string // DID
	Challenged      string // DID
	Status          string
	Color           string
	ProposedGameId  string
	TimeControl     *TimeControl
	Message         string
	CreatedAt       string
	ExpiresAt       string
}

// MaterialCount represents the material count for both sides
type MaterialCount struct {
	White int `json:"white"`
	Black int `json:"black"`
}

// PieceValues maps piece types to their standard values
var StandardPieceValues = map[string]int{
	"pawn":   1,
	"knight": 3,
	"bishop": 3,
	"rook":   5,
	"queen":  9,
	"king":   0, // King has no material value
}