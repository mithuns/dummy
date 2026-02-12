# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Run

```bash
cd tictactoe
go run main.go
```

The server starts at http://localhost:8080 (binds to 0.0.0.0:8080 for LAN access).

## Architecture

This is a multiplayer Tic-Tac-Toe game with a Go backend and HTMX frontend using Server-Sent Events (SSE) for real-time synchronization.

**Backend (tictactoe/main.go):**
- Single global `Game` struct holds board state, current turn, and winner
- SSE endpoint `/events` maintains persistent connections; clients receive HTML fragments on state changes
- `/move` and `/reset` endpoints modify game state and call `broadcastBoard()` to push updates to all connected clients
- Board HTML is rendered server-side via Go templates and sent as SSE data

**Frontend (tictactoe/index.html + style.css):**
- HTMX SSE extension (`hx-ext="sse"`) connects to `/events` and swaps received HTML into `#game-wrapper`
- Cell clicks trigger `hx-post="/move?cell={n}"` with `hx-swap="none"` (updates come via SSE, not the POST response)

**Key pattern:** All state lives on the server. The frontend never tracks game state—it just renders whatever HTML the server sends via SSE.
