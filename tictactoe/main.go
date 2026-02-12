package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
)

// Game holds the state of the Tic-Tac-Toe game.
type Game struct {
	Board  [9]string // "X", "O", or ""
	Turn   string    // "X" or "O"
	Winner string    // "X", "O", "Draw", or ""
	mu     sync.Mutex
}

// Global game instance
var game = &Game{
	Turn: "X",
}

// Client channels for SSE
// We use a map to store active client channels.
// A channel is just a way to send a string (HTML fragment) to a specific client connection.
var (
	clients   = make(map[chan string]bool)
	clientsMu sync.Mutex
)

// HTML Template for the board fragment.
// This is exactly what will be injected into the DOM via HTMX.
const boardTemplate = `
<div id="board-container" class="fade-in">
    <div class="status-bar">
        {{if ne .Winner ""}}
            <div class="winner-announcement">
                {{if eq .Winner "Draw"}}
                    <span>It's a Draw!</span>
                {{else}}
                    <span>Winner: {{.Winner}}!</span>
                {{end}}
                <button hx-post="/reset" hx-swap="none" class="btn reset-btn">Play Again</button>
            </div>
        {{else}}
            <div class="turn-indicator">Current Turn: <span class="player-{{.Turn}}">{{.Turn}}</span></div>
        {{end}}
    </div>

    <div class="board">
        {{range $i, $cell := .Board}}
        <button 
            class="cell {{if eq $cell "X"}}cell-x{{else if eq $cell "O"}}cell-o{{end}}"
            hx-post="/move?cell={{$i}}" 
            hx-swap="none"
            {{if ne $.Winner ""}}disabled{{end}}
            {{if ne $cell ""}}disabled{{end}}>
            {{$cell}}
        </button>
        {{end}}
    </div>
</div>
`

func main() {
	// Serve static files (specifically index.html and style.css)
	http.Handle("/", http.FileServer(http.Dir(".")))

	// Handle moves
	http.HandleFunc("/move", handleMove)

	// Handle reset
	http.HandleFunc("/reset", handleReset)

	// SSE Endpoint
	http.HandleFunc("/events", handleEvents)

	fmt.Println("Server started at http://0.0.0.0:8080")
	fmt.Println("Open http://localhost:8080 in your browser")
	// Bind to all interfaces for LAN access
	log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
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

	game.mu.Lock()
	// Logic to prevent move if game over or cell taken
	if game.Winner != "" || game.Board[cell] != "" {
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
	// Important for some proxies/browsers
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientChan := make(chan string, 1) // Buffer of 1 to prevent blocking

	clientsMu.Lock()
	clients[clientChan] = true
	clientsMu.Unlock()

	// Send initial state immediately upon connection
	initialHtml := renderBoardSafely()
	// Format as SSE event
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", strings.ReplaceAll(initialHtml, "\n", ""))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Clean up when client disconnects
	notify := r.Context().Done()

	go func() {
		<-notify
		clientsMu.Lock()
		delete(clients, clientChan)
		clientsMu.Unlock()
		close(clientChan)
	}()

	// Loop to send messages
	for msg := range clientChan {
		// Send the message as an SSE event "message"
		// HTMX by default swaps the content into the target when receiving a "message" event (if configured so)
		// We strip newlines from HTML to fit in one data line, or handle multi-line data appropriately.
		// For simplicity, we just strip/replace newlines.
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// broadcastBoard generates the current board HTML and sends it to all connected clients.
func broadcastBoard() {
	html := renderBoardSafely()
	// Compact the HTML for SSE transmission
	msg := strings.ReplaceAll(html, "\n", "")

	clientsMu.Lock()
	defer clientsMu.Unlock()

	for client := range clients {
		// Non-blocking send
		select {
		case client <- msg:
		default:
			// If client channel is full/blocked, skip to avoid holding up everyone
		}
	}
}

// renderBoardSafely handles locking and rendering the template
func renderBoardSafely() string {
	game.mu.Lock()
	defer game.mu.Unlock()

	tmpl, err := template.New("board").Parse(boardTemplate)
	if err != nil {
		log.Printf("Template error: %v", err)
		return "<div>Error rendering board</div>"
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, game)
	if err != nil {
		log.Printf("Template execution error: %v", err)
		return "<div>Error executing template</div>"
	}
	return buf.String()
}

func checkWinner(player string) bool {
	// Check lines
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
