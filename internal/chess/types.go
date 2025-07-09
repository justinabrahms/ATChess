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
	From      string
	To        string
	SAN       string
	FEN       string
	Check     bool
	Checkmate bool
	Draw      bool
	GameOver  bool
	Result    string
}

type Game struct {
	ID          string
	White       string // DID
	Black       string // DID
	Status      GameStatus
	FEN         string
	PGN         string
	TimeControl *TimeControl
	CreatedAt   string
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