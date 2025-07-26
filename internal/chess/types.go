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
	ID          string      `json:"id"`
	White       string      `json:"white"` // DID
	Black       string      `json:"black"` // DID
	Status      GameStatus  `json:"status"`
	FEN         string      `json:"fen"`
	PGN         string      `json:"pgn"`
	TimeControl *TimeControl `json:"timeControl"`
	CreatedAt   string      `json:"createdAt"`
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