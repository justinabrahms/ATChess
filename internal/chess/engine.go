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
	
	// Check for automatic draws after the move
	isDraw := e.game.Outcome() == chess.Draw
	gameOver := e.game.Outcome() != chess.NoOutcome
	
	result := &MoveResult{
		From:      from,
		To:        to,
		SAN:       san,
		FEN:       positionAfter.String(),
		Check:     isCheck,
		Checkmate: isCheckmate,
		Draw:      isDraw,
		GameOver:  gameOver,
	}
	
	// Set the result string based on the outcome
	if e.game.Outcome() != chess.NoOutcome {
		result.Result = e.game.Outcome().String()
		
		// Add draw reason to result if it's a draw
		if isDraw && e.GetDrawReason() != "" {
			result.Result = result.Result + " - " + e.GetDrawReason()
		}
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

// IsDrawn returns true if the game has ended in a draw for any reason
func (e *Engine) IsDrawn() bool {
	return e.game.Outcome() == chess.Draw
}

// GetDrawMethod returns the method by which the game was drawn, or NoMethod if not drawn
func (e *Engine) GetDrawMethod() chess.Method {
	if e.game.Outcome() == chess.Draw {
		return e.game.Method()
	}
	return chess.NoMethod
}

// IsStalemate returns true if the current position is a stalemate
func (e *Engine) IsStalemate() bool {
	return e.game.Method() == chess.Stalemate
}

// IsThreefoldRepetition returns true if the position has been repeated three times
// Note: This requires a player to claim the draw using ClaimDraw()
func (e *Engine) IsThreefoldRepetition() bool {
	for _, method := range e.game.EligibleDraws() {
		if method == chess.ThreefoldRepetition {
			return true
		}
	}
	return false
}

// IsFivefoldRepetition returns true if the game was automatically drawn by fivefold repetition
func (e *Engine) IsFivefoldRepetition() bool {
	return e.game.Method() == chess.FivefoldRepetition
}

// IsFiftyMoveRule returns true if 50 moves have been made without a pawn move or capture
// Note: This requires a player to claim the draw using ClaimDraw()
func (e *Engine) IsFiftyMoveRule() bool {
	for _, method := range e.game.EligibleDraws() {
		if method == chess.FiftyMoveRule {
			return true
		}
	}
	return false
}

// IsSeventyFiveMoveRule returns true if the game was automatically drawn by the 75-move rule
func (e *Engine) IsSeventyFiveMoveRule() bool {
	return e.game.Method() == chess.SeventyFiveMoveRule
}

// IsInsufficientMaterial returns true if the game was drawn due to insufficient material
func (e *Engine) IsInsufficientMaterial() bool {
	return e.game.Method() == chess.InsufficientMaterial
}

// GetEligibleDraws returns the draw methods that can be claimed by the current player
func (e *Engine) GetEligibleDraws() []chess.Method {
	return e.game.EligibleDraws()
}

// ClaimDraw attempts to claim a draw by the specified method
// This is used for draws that require a player to claim them (threefold repetition, fifty-move rule)
func (e *Engine) ClaimDraw(method chess.Method) error {
	return e.game.Draw(method)
}

// GetDrawReason returns a human-readable reason for why the game is drawn
func (e *Engine) GetDrawReason() string {
	if !e.IsDrawn() {
		return ""
	}
	
	switch e.game.Method() {
	case chess.Stalemate:
		return "Stalemate - Player has no legal moves but is not in check"
	case chess.ThreefoldRepetition:
		return "Draw by threefold repetition"
	case chess.FivefoldRepetition:
		return "Automatic draw by fivefold repetition"
	case chess.FiftyMoveRule:
		return "Draw by fifty-move rule"
	case chess.SeventyFiveMoveRule:
		return "Automatic draw by seventy-five-move rule"
	case chess.InsufficientMaterial:
		return "Draw by insufficient material to checkmate"
	case chess.DrawOffer:
		return "Draw by agreement"
	default:
		return "Draw"
	}
}

// GetPieceValues returns a map of piece types to their standard values
func (e *Engine) GetPieceValues() map[string]int {
	return StandardPieceValues
}

// GetMaterialCount returns the material count for both white and black
func (e *Engine) GetMaterialCount() MaterialCount {
	count := MaterialCount{White: 0, Black: 0}
	position := e.game.Position()
	board := position.Board()
	
	// Iterate through all squares on the board
	for sq := chess.A1; sq <= chess.H8; sq++ {
		piece := board.Piece(sq)
		if piece == chess.NoPiece {
			continue
		}
		
		// Get piece value
		value := getPieceValue(piece.Type())
		
		// Add to appropriate color's count
		if piece.Color() == chess.White {
			count.White += value
		} else {
			count.Black += value
		}
	}
	
	return count
}

// GetMaterialBalance returns the material difference (positive = white advantage)
func (e *Engine) GetMaterialBalance() int {
	count := e.GetMaterialCount()
	return count.White - count.Black
}

// getPieceValue returns the material value for a piece type
func getPieceValue(pieceType chess.PieceType) int {
	switch pieceType {
	case chess.Pawn:
		return 1
	case chess.Knight:
		return 3
	case chess.Bishop:
		return 3
	case chess.Rook:
		return 5
	case chess.Queen:
		return 9
	case chess.King:
		return 0
	default:
		return 0
	}
}


func parseSquare(sq string) chess.Square {
	if len(sq) != 2 {
		return chess.NoSquare
	}
	
	file := sq[0] - 'a'
	rank := sq[1] - '1'
	
	if file > 7 || rank > 7 {
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