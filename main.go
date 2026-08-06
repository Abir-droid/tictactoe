package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/gorilla/websocket"
)

// --- DATA STRUCTURES ---

type RegisterRequest struct {
	Username string `json:"username"`
}

type Message struct {
	Type     string    `json:"type"`               // "JOIN_QUEUE", "MOVE", "MATCH_FOUND", "GAME_STATE", "GAME_OVER", "RETRY_REQUESTED", "RESTART_GAME", "OPPONENT_LEFT", "ERROR"
	Position int       `json:"position,omitempty"` // 0 to 8
	Symbol   string    `json:"symbol,omitempty"`   // "X" or "O"
	Turn     string    `json:"turn,omitempty"`     // "X" or "O"
	Winner   string    `json:"winner,omitempty"`   // "X", "O", "DRAW", or ""
	Board    [9]string `json:"board"`
	Opponent string    `json:"opponent,omitempty"`
	Error    string    `json:"error,omitempty"`
	Message  string    `json:"message,omitempty"`
}

type Client struct {
	Username     string
	Conn         *websocket.Conn
	Send         chan Message
	Room         *GameRoom
	Symbol       string
	LastOpponent string
}

type GameRoom struct {
	ID       string
	PlayerX  *Client
	PlayerO  *Client
	Board    [9]string
	Turn     string
	GameOver bool
	RetryX   bool
	RetryO   bool
	Mu       sync.Mutex
}

// --- GLOBAL STATE ---

var (
	users      = make(map[string]bool)
	usersMu    sync.Mutex
	matchQueue = make(chan *Client, 100)
	leaveQueue = make(chan *Client, 100)
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	go matchmaker()

	http.HandleFunc("/api/register", handleRegister)
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 Game server running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// --- HTTP HANDLERS ---

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		http.Error(w, "Invalid username", http.StatusBadRequest)
		return
	}

	usersMu.Lock()
	defer usersMu.Unlock()

	if users[req.Username] {
		http.Error(w, "Username already taken", http.StatusConflict)
		return
	}

	users[req.Username] = true
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "registered", "username": req.Username})
}

// --- WEBSOCKET & BULLETPROOF MATCHMAKING ---

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	usersMu.Lock()
	registered := users[username]
	usersMu.Unlock()

	if !registered {
		http.Error(w, "Unauthorized player", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	client := &Client{
		Username: username,
		Conn:     conn,
		Send:     make(chan Message, 20),
	}

	go client.writePump()
	go client.readPump()
}

func (c *Client) safeSend(msg Message) {
	select {
	case c.Send <- msg:
	default:
		log.Printf("Warning: Send channel full for user %s\n", c.Username)
	}
}

func matchmaker() {
	var waiting []*Client

	for {
		select {
		case client := <-matchQueue:
			// Remove any old/duplicate instances of this user or pointer from queue
			cleaned := make([]*Client, 0, len(waiting))
			for _, w := range waiting {
				if w != client && w.Username != client.Username {
					cleaned = append(cleaned, w)
				}
			}
			waiting = append(cleaned, client)
			waiting = processMatches(waiting)

		case client := <-leaveQueue:
			cleaned := make([]*Client, 0, len(waiting))
			for _, w := range waiting {
				if w != client && w.Username != client.Username {
					cleaned = append(cleaned, w)
				}
			}
			waiting = cleaned
		}
	}
}

func processMatches(waiting []*Client) []*Client {
	for len(waiting) >= 2 {
		p1 := waiting[0]
		matchedIdx := -1

		// Priority 1: Search for an opponent with a DIFFERENT username who is NOT p1's last opponent
		for i := 1; i < len(waiting); i++ {
			if waiting[i].Username != p1.Username && waiting[i].Username != p1.LastOpponent {
				matchedIdx = i
				break
			}
		}

		// Priority 2: Fallback to matching with last opponent if no other player exists
		if matchedIdx == -1 {
			for i := 1; i < len(waiting); i++ {
				if waiting[i].Username != p1.Username {
					matchedIdx = i
					break
				}
			}
		}

		// Stop if no valid distinct opponent is found
		if matchedIdx == -1 {
			break
		}

		p2 := waiting[matchedIdx]

		// Remove p1 and p2 from waiting list
		newWaiting := make([]*Client, 0, len(waiting)-2)
		for idx, w := range waiting {
			if idx != 0 && idx != matchedIdx {
				newWaiting = append(newWaiting, w)
			}
		}
		waiting = newWaiting

		startMatch(p1, p2)
	}

	return waiting
}

func startMatch(p1, p2 *Client) {
	p1.LastOpponent = p2.Username
	p2.LastOpponent = p1.Username

	room := &GameRoom{
		ID:      fmt.Sprintf("%s_vs_%s", p1.Username, p2.Username),
		PlayerX: p1,
		PlayerO: p2,
		Turn:    "X",
	}

	p1.Room = room
	p1.Symbol = "X"

	p2.Room = room
	p2.Symbol = "O"

	p1.safeSend(Message{Type: "MATCH_FOUND", Symbol: "X", Turn: "X", Opponent: p2.Username})
	p2.safeSend(Message{Type: "MATCH_FOUND", Symbol: "O", Turn: "X", Opponent: p1.Username})
}

// --- GAME & ROOM LIFECYCLE LOGIC ---

func (c *Client) readPump() {
	defer func() {
		// Clean up username on disconnect so player can re-register/refresh
		usersMu.Lock()
		delete(users, c.Username)
		usersMu.Unlock()

		leaveQueue <- c
		c.leaveCurrentRoom()
		c.Conn.Close()
	}()

	for {
		var msg Message
		err := c.Conn.ReadJSON(&msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "JOIN_QUEUE":
			c.leaveCurrentRoom()
			matchQueue <- c

		case "MOVE":
			if c.Room != nil {
				c.Room.handleMove(c, msg.Position)
			}

		case "RETRY":
			if c.Room != nil {
				c.Room.handleRetry(c)
			}
		}
	}
}

func (c *Client) writePump() {
	defer c.Conn.Close()
	for msg := range c.Send {
		err := c.Conn.WriteJSON(msg)
		if err != nil {
			break
		}
	}
}

func (c *Client) leaveCurrentRoom() {
	if c.Room == nil {
		return
	}

	r := c.Room
	r.Mu.Lock()
	defer r.Mu.Unlock()

	var opponent *Client
	if c == r.PlayerX {
		opponent = r.PlayerO
	} else if c == r.PlayerO {
		opponent = r.PlayerX
	}

	if opponent != nil && opponent.Room == r {
		opponent.Room = nil
		opponent.safeSend(Message{
			Type:    "OPPONENT_LEFT",
			Message: "Your opponent left the game.",
		})
	}

	c.Room = nil
}

func (r *GameRoom) handleMove(c *Client, pos int) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.GameOver {
		c.safeSend(Message{Type: "ERROR", Error: "Game is already over"})
		return
	}

	if pos < 0 || pos > 8 || r.Board[pos] != "" {
		c.safeSend(Message{Type: "ERROR", Error: "Invalid position"})
		return
	}

	if c.Symbol != r.Turn {
		c.safeSend(Message{Type: "ERROR", Error: "Not your turn"})
		return
	}

	r.Board[pos] = c.Symbol

	winner := checkWinner(r.Board)
	if winner != "" {
		r.GameOver = true
		stateMsg := Message{
			Type:   "GAME_OVER",
			Board:  r.Board,
			Winner: winner,
		}
		r.PlayerX.safeSend(stateMsg)
		r.PlayerO.safeSend(stateMsg)
		return
	}

	if r.Turn == "X" {
		r.Turn = "O"
	} else {
		r.Turn = "X"
	}

	broadcastMsg := Message{
		Type:  "GAME_STATE",
		Board: r.Board,
		Turn:  r.Turn,
	}
	r.PlayerX.safeSend(broadcastMsg)
	r.PlayerO.safeSend(broadcastMsg)
}

func (r *GameRoom) handleRetry(c *Client) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if !r.GameOver {
		return
	}

	if c == r.PlayerX {
		r.RetryX = true
		r.PlayerO.safeSend(Message{Type: "RETRY_REQUESTED", Message: "Opponent wants a rematch!"})
	} else if c == r.PlayerO {
		r.RetryO = true
		r.PlayerX.safeSend(Message{Type: "RETRY_REQUESTED", Message: "Opponent wants a rematch!"})
	}

	if r.RetryX && r.RetryO {
		r.Board = [9]string{}
		r.GameOver = false
		r.RetryX = false
		r.RetryO = false

		r.PlayerX, r.PlayerO = r.PlayerO, r.PlayerX
		r.PlayerX.Symbol = "X"
		r.PlayerO.Symbol = "O"
		r.Turn = "X"

		r.PlayerX.safeSend(Message{Type: "RESTART_GAME", Symbol: "X", Turn: "X", Board: r.Board})
		r.PlayerO.safeSend(Message{Type: "RESTART_GAME", Symbol: "O", Turn: "X", Board: r.Board})
	}
}

func checkWinner(b [9]string) string {
	lines := [8][3]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8},
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8},
		{0, 4, 8}, {2, 4, 6},
	}

	for _, l := range lines {
		if b[l[0]] != "" && b[l[0]] == b[l[1]] && b[l[0]] == b[l[2]] {
			return b[l[0]]
		}
	}

	full := true
	for _, cell := range b {
		if cell == "" {
			full = false
			break
		}
	}
	if full {
		return "DRAW"
	}

	return ""
}