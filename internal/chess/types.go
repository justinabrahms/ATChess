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
	Initial   int // seconds
	Increment int // seconds per move
}

type Challenge struct {
	ID          string
	Challenger  string // DID
	Challenged  string // DID
	Status      string
	Color       string
	TimeControl *TimeControl
	Message     string
	CreatedAt   string
	ExpiresAt   string
}