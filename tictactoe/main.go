package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
)

// Game holds the state of the Tic-Tac-Toe game.
type Game struct {
	Board   [9]string // "X", "O", or ""
	Turn    string    // "X" or "O"
	Winner  string    // "X", "O", "Draw", or ""
	PlayerX string    // session ID of player X
	PlayerO string    // session ID of player O
	mu      sync.Mutex
}

// BoardData is passed to the template for per-player rendering.
type BoardData struct {
	Board      [9]string
	Turn       string
	Winner     string
	Role       string // "X", "O", or "" (spectator)
	MyTurn     bool
	BothJoined bool
}

// Global game instance
var game = &Game{
	Turn: "X",
}

// Client channels for SSE: maps channel -> player session ID
var (
	clients   = make(map[chan string]string)
	clientsMu sync.Mutex
)

// HTML Template for the board fragment.
// Each client receives a personalized version based on their assigned role.
const boardTemplate = `
<div id="board-container" class="fade-in">
    <div class="status-bar">
        {{if ne .Role ""}}
            <div class="role-indicator">You are: <span class="player-{{.Role}}">{{.Role}}</span></div>
        {{else}}
            <div class="role-indicator">Spectator</div>
        {{end}}

        {{if ne .Winner ""}}
            <div class="winner-announcement">
                {{if eq .Winner "Draw"}}
                    <span>It's a Draw!</span>
                {{else}}
                    <span>Winner: {{.Winner}}!</span>
                {{end}}
                <button hx-post="/reset" hx-swap="none" class="btn reset-btn">Play Again</button>
            </div>
        {{else if not .BothJoined}}
            <div class="turn-indicator">Waiting for opponent to join...</div>
        {{else if .MyTurn}}
            <div class="turn-indicator">Your turn! (<span class="player-{{.Turn}}">{{.Turn}}</span>)</div>
        {{else}}
            <div class="turn-indicator">Waiting for <span class="player-{{.Turn}}">{{.Turn}}</span>...</div>
        {{end}}
    </div>

    <div class="board">
        {{range $i, $cell := .Board}}
        <button
            class="cell {{if eq $cell "X"}}cell-x{{else if eq $cell "O"}}cell-o{{end}}"
            hx-post="/move?cell={{$i}}"
            hx-swap="none"
            {{if or (ne $.Winner "") (ne $cell "") (not $.MyTurn) (not $.BothJoined)}}disabled{{end}}>
            {{$cell}}
        </button>
        {{end}}
    </div>
</div>
`

func main() {
	// Serve static files with cookie middleware
	http.Handle("/", withPlayerCookie(http.FileServer(http.Dir("."))))

	// Handle moves
	http.HandleFunc("/move", handleMove)

	// Handle reset
	http.HandleFunc("/reset", handleReset)

	// SSE Endpoint
	http.HandleFunc("/events", handleEvents)

	port := "8080"
	fmt.Printf("Server started at http://0.0.0.0:%s\n", port)

	// Find and print LAN IP
	if ip := getOutboundIP(); ip != "" {
		fmt.Printf("Play with friends at: http://%s:%s\n", ip, port)
	} else {
		fmt.Printf("Could not detect LAN IP. Try sharing your machine's IP manually.\n")
	}

	fmt.Printf("Localhost: http://localhost:%s\n", port)

	// Bind to all interfaces for LAN access
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// withPlayerCookie sets a player_id cookie if one is not already present.
func withPlayerCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("player_id"); err != nil {
			b := make([]byte, 16)
			rand.Read(b)
			http.SetCookie(w, &http.Cookie{
				Name:  "player_id",
				Value: hex.EncodeToString(b),
				Path:  "/",
			})
		}
		next.ServeHTTP(w, r)
	})
}

func getPlayerID(r *http.Request) string {
	cookie, err := r.Cookie("player_id")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// getOutboundIP returns the preferred outbound IP of this machine.
func getOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cellStr := r.URL.Query().Get("cell")
	cell, err := strconv.Atoi(cellStr)
	if err != nil || cell < 0 || cell > 8 {
		http.Error(w, "Invalid cell", http.StatusBadRequest)
		return
	}

	playerID := getPlayerID(r)

	game.mu.Lock()
	// Only the player whose turn it is can move
	allowed := (game.Turn == "X" && game.PlayerX == playerID) ||
		(game.Turn == "O" && game.PlayerO == playerID)

	if !allowed || game.Winner != "" || game.Board[cell] != "" {
		game.mu.Unlock()
		return
	}

	// Apply move
	game.Board[cell] = game.Turn

	// Check for winner
	if checkWinner(game.Turn) {
		game.Winner = game.Turn
	} else if isDraw() {
		game.Winner = "Draw"
	} else {
		// Switch turn
		if game.Turn == "X" {
			game.Turn = "O"
		} else {
			game.Turn = "X"
		}
	}
	game.mu.Unlock()

	// Broadcast update to all clients
	broadcastBoard()
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	game.mu.Lock()
	game.Board = [9]string{}
	game.Turn = "X"
	game.Winner = ""
	game.mu.Unlock()

	broadcastBoard()
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	// Mandatory headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	playerID := getPlayerID(r)

	// Assign a role if one is available
	game.mu.Lock()
	newRoleAssigned := false
	if playerID != "" {
		if game.PlayerX == "" || game.PlayerX == playerID {
			game.PlayerX = playerID
			newRoleAssigned = true
		} else if game.PlayerO == "" || game.PlayerO == playerID {
			game.PlayerO = playerID
			newRoleAssigned = true
		}
	}
	game.mu.Unlock()

	clientChan := make(chan string, 1)

	clientsMu.Lock()
	clients[clientChan] = playerID
	clientsMu.Unlock()

	// Send initial state for this player
	initialHTML := renderBoardForPlayer(playerID)
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", strings.ReplaceAll(initialHTML, "\n", ""))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// If a new role was assigned, broadcast so existing clients update
	// (e.g. "Waiting for opponent" -> "Your turn!")
	if newRoleAssigned {
		broadcastBoard()
	}

	// Clean up when client disconnects
	notify := r.Context().Done()

	go func() {
		<-notify

		clientsMu.Lock()
		pid := clients[clientChan]
		delete(clients, clientChan)

		// Check if any other connection shares this player ID
		hasOtherConn := false
		for _, id := range clients {
			if id == pid {
				hasOtherConn = true
				break
			}
		}
		clientsMu.Unlock()

		// If no other connections with this ID, free up their role
		if !hasOtherConn && pid != "" {
			game.mu.Lock()
			if game.PlayerX == pid {
				game.PlayerX = ""
			} else if game.PlayerO == pid {
				game.PlayerO = ""
			}
			game.mu.Unlock()
			broadcastBoard()
		}

		close(clientChan)
	}()

	// Loop to send messages
	for msg := range clientChan {
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// renderBoardForPlayer renders the board HTML customized for a specific player.
func renderBoardForPlayer(playerID string) string {
	game.mu.Lock()
	data := BoardData{
		Board:      game.Board,
		Turn:       game.Turn,
		Winner:     game.Winner,
		BothJoined: game.PlayerX != "" && game.PlayerO != "",
	}
	if playerID != "" {
		switch playerID {
		case game.PlayerX:
			data.Role = "X"
			data.MyTurn = game.Turn == "X" && game.Winner == ""
		case game.PlayerO:
			data.Role = "O"
			data.MyTurn = game.Turn == "O" && game.Winner == ""
		}
	}
	if !data.BothJoined {
		data.MyTurn = false
	}
	game.mu.Unlock()

	tmpl, err := template.New("board").Parse(boardTemplate)
	if err != nil {
		log.Printf("Template error: %v", err)
		return "<div>Error rendering board</div>"
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("Template execution error: %v", err)
		return "<div>Error executing template</div>"
	}
	return buf.String()
}

// broadcastBoard sends a personalized board to each connected client.
func broadcastBoard() {
	game.mu.Lock()
	baseData := BoardData{
		Board:      game.Board,
		Turn:       game.Turn,
		Winner:     game.Winner,
		BothJoined: game.PlayerX != "" && game.PlayerO != "",
	}
	playerXID := game.PlayerX
	playerOID := game.PlayerO
	game.mu.Unlock()

	tmpl, err := template.New("board").Parse(boardTemplate)
	if err != nil {
		log.Printf("Template error: %v", err)
		return
	}

	clientsMu.Lock()
	defer clientsMu.Unlock()

	for client, pid := range clients {
		data := baseData
		if pid != "" {
			switch pid {
			case playerXID:
				data.Role = "X"
				data.MyTurn = baseData.Turn == "X" && baseData.Winner == ""
			case playerOID:
				data.Role = "O"
				data.MyTurn = baseData.Turn == "O" && baseData.Winner == ""
			}
		}
		if !data.BothJoined {
			data.MyTurn = false
		}

		var buf bytes.Buffer
		tmpl.Execute(&buf, data)
		msg := strings.ReplaceAll(buf.String(), "\n", "")

		select {
		case client <- msg:
		default:
			select {
			case <-client:
			default:
			}
			select {
			case client <- msg:
			default:
			}
		}
	}
}

func checkWinner(player string) bool {
	// Rows
	for i := 0; i < 9; i += 3 {
		if game.Board[i] == player && game.Board[i+1] == player && game.Board[i+2] == player {
			return true
		}
	}
	// Cols
	for i := 0; i < 3; i++ {
		if game.Board[i] == player && game.Board[i+3] == player && game.Board[i+6] == player {
			return true
		}
	}
	// Diags
	if game.Board[0] == player && game.Board[4] == player && game.Board[8] == player {
		return true
	}
	if game.Board[2] == player && game.Board[4] == player && game.Board[6] == player {
		return true
	}
	return false
}

func isDraw() bool {
	for _, cell := range game.Board {
		if cell == "" {
			return false
		}
	}
	return true
}
