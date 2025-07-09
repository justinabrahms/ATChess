package chess

import (
	"fmt"

	"github.com/notnil/chess"
)

type Engine struct {
	game *chess.Game
}

func NewEngine() *Engine {
	return &Engine{
		game: chess.NewGame(),
	}
}

func NewEngineFromFEN(fen string) (*Engine, error) {
	fenFunc, err := chess.FEN(fen)
	if err != nil {
		return nil, fmt.Errorf("invalid FEN: %w", err)
	}
	
	return &Engine{
		game: chess.NewGame(fenFunc),
	}, nil
}

func (e *Engine) MakeMove(from, to string, promotion chess.PieceType) (*MoveResult, error) {
	fromSquare := parseSquare(from)
	toSquare := parseSquare(to)
	
	if fromSquare == chess.NoSquare || toSquare == chess.NoSquare {
		return nil, fmt.Errorf("invalid square notation")
	}
	
	// Create move
	var move *chess.Move
	if promotion != chess.NoPieceType {
		move = &chess.Move{
			S1:    fromSquare,
			S2:    toSquare,
			Promo: promotion,
		}
	} else {
		move = &chess.Move{
			S1: fromSquare,
			S2: toSquare,
		}
	}
	
	// Validate move
	validMoves := e.game.ValidMoves()
	var validMove *chess.Move
	for _, vm := range validMoves {
		if vm.S1() == move.S1 && vm.S2() == move.S2 && vm.Promo() == move.Promo {
			validMove = vm
			break
		}
	}
	
	if validMove == nil {
		return nil, fmt.Errorf("invalid move: %s to %s", from, to)
	}
	
	// Make the move
	if err := e.game.Move(validMove); err != nil {
		return nil, fmt.Errorf("failed to make move: %w", err)
	}
	
	// Get position after move
	position := e.game.Position()
	
	result := &MoveResult{
		From:      from,
		To:        to,
		SAN:       chess.AlgebraicNotation{}.Encode(position, validMove),
		FEN:       chess.FEN(e.game.Position()),
		Check:     position.InCheck(),
		Checkmate: e.game.Method() == chess.Checkmate,
		Draw:      e.game.Outcome() == chess.Draw,
		GameOver:  e.game.Outcome() != chess.NoOutcome,
	}
	
	if e.game.Outcome() != chess.NoOutcome {
		result.Result = e.game.Outcome().String()
	}
	
	return result, nil
}

func (e *Engine) GetFEN() string {
	return chess.FEN(e.game.Position())
}

func (e *Engine) GetPGN() string {
	return chess.PGN(e.game)
}

func (e *Engine) GetStatus() GameStatus {
	switch e.game.Outcome() {
	case chess.WhiteWon:
		return StatusWhiteWon
	case chess.BlackWon:
		return StatusBlackWon
	case chess.Draw:
		return StatusDraw
	default:
		return StatusActive
	}
}

func (e *Engine) GetActiveColor() string {
	if e.game.Position().Turn() == chess.White {
		return "white"
	}
	return "black"
}

func (e *Engine) ValidateFEN(fen string) error {
	_, err := chess.FEN(fen)
	return err
}

func parseSquare(sq string) chess.Square {
	if len(sq) != 2 {
		return chess.NoSquare
	}
	
	file := sq[0] - 'a'
	rank := sq[1] - '1'
	
	if file < 0 || file > 7 || rank < 0 || rank > 7 {
		return chess.NoSquare
	}
	
	return chess.Square(rank*8 + file)
}

func ParsePromotion(p string) chess.PieceType {
	switch p {
	case "q":
		return chess.Queen
	case "r":
		return chess.Rook
	case "b":
		return chess.Bishop
	case "n":
		return chess.Knight
	default:
		return chess.NoPieceType
	}
}