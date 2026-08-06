package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// --- GLOBAL DATABASE HANDLE ---
var db *sql.DB

// --- DATA STRUCTURES ---

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
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
	matchQueue = make(chan *Client, 100)
	leaveQueue = make(chan *Client, 100)
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	initDB()
	go matchmaker()

	http.HandleFunc("/api/register", handleRegister)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 Game server running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// --- DATABASE CONNECTION & SETUP ---

func initDB() {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Println("⚠️ DATABASE_URL not set, database features will fail if not provided.")
		return
	}

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Failed to connect to PostgreSQL:", err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal("PostgreSQL ping failed:", err)
	}

	fmt.Println("✅ Connected to PostgreSQL database successfully!")

	// Creates users table automatically if it doesn't exist
	tableQuery := `
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(50) UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		wins INT DEFAULT 0,
		losses INT DEFAULT 0,
		draws INT DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	_, err = db.Exec(tableQuery)
	if err != nil {
		log.Fatal("Failed to create users table:", err)
	}
}

// --- AUTH HANDLERS ---

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		http.Error(w, "Username and password required", http.StatusBadRequest)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec("INSERT INTO users (username, password_hash) VALUES ($1, $2)", req.Username, string(hashedPassword))
	if err != nil {
		http.Error(w, "Username already exists", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "registered", "username": req.Username})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	var storedHash string
	err := db.QueryRow("SELECT password_hash FROM users WHERE username = $1", req.Username).Scan(&storedHash)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password)); err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "authenticated", "username": req.Username})
}

// --- WEBSOCKET HANDLER ---

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if username == "" {
		http.Error(w, "Username required", http.StatusBadRequest)
		return
	}

	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", username).Scan(&exists)
	if err != nil || !exists {
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

		for i := 1; i < len(waiting); i++ {
			if waiting[i].Username != p1.Username && waiting[i].Username != p1.LastOpponent {
				matchedIdx = i
				break
			}
		}

		if matchedIdx == -1 {
			for i := 1; i < len(waiting); i++ {
				if waiting[i].Username != p1.Username {
					matchedIdx = i
					break
				}
			}
		}

		if matchedIdx == -1 {
			break
		}

		p2 := waiting[matchedIdx]

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

// --- GAME LIFECYCLE & STATS RECORDING ---

func (c *Client) readPump() {
	defer func() {
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

		// Record match result in PostgreSQL without blocking gameplay
		go saveGameResult(r.PlayerX.Username, r.PlayerO.Username, winner)
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

func saveGameResult(userX, userO, winner string) {
	if db == nil {
		return
	}

	if winner == "X" {
		_, _ = db.Exec("UPDATE users SET wins = wins + 1 WHERE username = $1", userX)
		_, _ = db.Exec("UPDATE users SET losses = losses + 1 WHERE username = $1", userO)
	} else if winner == "O" {
		_, _ = db.Exec("UPDATE users SET wins = wins + 1 WHERE username = $1", userO)
		_, _ = db.Exec("UPDATE users SET losses = losses + 1 WHERE username = $1", userX)
	} else if winner == "DRAW" {
		_, _ = db.Exec("UPDATE users SET draws = draws + 1 WHERE username = $1", userX)
		_, _ = db.Exec("UPDATE users SET draws = draws + 1 WHERE username = $1", userO)
	}
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