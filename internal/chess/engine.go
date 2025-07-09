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
	
	// Validate move
	validMoves := e.game.ValidMoves()
	var validMove *chess.Move
	for _, vm := range validMoves {
		if vm.S1() == fromSquare && vm.S2() == toSquare && vm.Promo() == promotion {
			validMove = vm
			break
		}
	}
	
	if validMove == nil {
		return nil, fmt.Errorf("invalid move: %s to %s", from, to)
	}
	
	// Get position before move for proper SAN notation
	positionBefore := e.game.Position()
	
	// Make the move
	if err := e.game.Move(validMove); err != nil {
		return nil, fmt.Errorf("failed to make move: %w", err)
	}
	
	// Get position after move
	positionAfter := e.game.Position()
	
	san := chess.AlgebraicNotation{}.Encode(positionBefore, validMove)
	
	isCheck := len(san) > 0 && (san[len(san)-1] == '+' || san[len(san)-1] == '#')
	isCheckmate := len(san) > 0 && san[len(san)-1] == '#'
	
	result := &MoveResult{
		From:      from,
		To:        to,
		SAN:       san,
		FEN:       positionAfter.String(),
		Check:     isCheck,
		Checkmate: isCheckmate,
		Draw:      e.game.Outcome() == chess.Draw,
		GameOver:  e.game.Outcome() != chess.NoOutcome,
	}
	
	if e.game.Outcome() != chess.NoOutcome {
		result.Result = e.game.Outcome().String()
	}
	
	return result, nil
}

func (e *Engine) GetFEN() string {
	return e.game.Position().String()
}

func (e *Engine) GetPGN() string {
	return e.game.String()
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
	fenFunc, err := chess.FEN(fen)
	if err != nil {
		return err
	}
	// Test if the FEN can be used to create a game
	_ = chess.NewGame(fenFunc)
	return nil
}

func (e *Engine) isInCheck() bool {
	// For now, we'll rely on the SAN notation to indicate check
	// This method is not currently used in the main logic
	return false
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
	
	return chess.Square(int(rank)*8 + int(file))
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