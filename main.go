package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

var db *sql.DB

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LeaderboardEntry struct {
	Username string `json:"username"`
	Wins     int    `json:"wins"`
	Losses   int    `json:"losses"`
	Draws    int    `json:"draws"`
	Total    int    `json:"total"`
}

type UserStats struct {
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Draws  int `json:"draws"`
	Total  int `json:"total"`
}

type Message struct {
	Type     string    `json:"type"` // "JOIN_QUEUE", "LEAVE_QUEUE", "MOVE", "MATCH_FOUND", "GAME_STATE", "GAME_OVER", "RETRY_REQUESTED", "RESTART_GAME", "OPPONENT_LEFT", "CHAT", "ERROR"
	Position int       `json:"position,omitempty"`
	Symbol   string    `json:"symbol,omitempty"`
	Turn     string    `json:"turn,omitempty"`
	Winner   string    `json:"winner,omitempty"`
	Board    [9]string `json:"board"`
	Opponent string    `json:"opponent,omitempty"`
	Error    string    `json:"error,omitempty"`
	Message  string    `json:"message,omitempty"`
	Sender   string    `json:"sender,omitempty"`
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

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/api/register", handleRegister)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/leaderboard", handleLeaderboard)
	http.HandleFunc("/api/profile", handleProfile)
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 Game server running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

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

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		log.Fatal("PostgreSQL ping failed:", err)
	}

	fmt.Println("✅ Connected to PostgreSQL database successfully!")

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

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if db != nil {
		var dummy int
		err := db.QueryRow("SELECT 1").Scan(&dummy)
		if err != nil {
			http.Error(w, "Database ping failed", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

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

func handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rows, err := db.Query("SELECT username, wins, losses, draws, (wins + losses + draws) AS total FROM users ORDER BY wins DESC, draws DESC, losses ASC LIMIT 10")
	if err != nil {
		http.Error(w, "Failed to fetch leaderboard", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	leaderboard := make([]LeaderboardEntry, 0)
	for rows.Next() {
		var entry LeaderboardEntry
		if err := rows.Scan(&entry.Username, &entry.Wins, &entry.Losses, &entry.Draws, &entry.Total); err == nil {
			leaderboard = append(leaderboard, entry)
		}
	}

	if err := rows.Err(); err != nil {
		http.Error(w, "Error iterating leaderboard rows", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(leaderboard)
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		http.Error(w, "Username required", http.StatusBadRequest)
		return
	}

	var stats UserStats
	err := db.QueryRow("SELECT wins, losses, draws, (wins + losses + draws) AS total FROM users WHERE username = $1", username).Scan(&stats.Wins, &stats.Losses, &stats.Draws, &stats.Total)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

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

func (c *Client) readPump() {
	defer func() {
		leaveQueue <- c
		c.leaveCurrentRoom()
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(512)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

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

		case "LEAVE_QUEUE":
			c.leaveCurrentRoom()
			leaveQueue <- c

		case "MOVE":
			if c.Room != nil {
				c.Room.handleMove(c, msg.Position)
			}

		case "RETRY":
			if c.Room != nil {
				c.Room.handleRetry(c)
			}

		case "CHAT":
			if c.Room != nil && msg.Message != "" {
				c.Room.handleChat(c, msg.Message)
			}
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
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

	switch c {
	case r.PlayerX:
		opponent = r.PlayerO
	case r.PlayerO:
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

func (r *GameRoom) handleChat(c *Client, text string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	chatMsg := Message{
		Type:    "CHAT",
		Message: text,
		Sender:  c.Username,
	}

	r.PlayerX.safeSend(chatMsg)
	r.PlayerO.safeSend(chatMsg)
}

func saveGameResult(userX, userO, winner string) {
	if db == nil {
		return
	}

	switch winner {
	case "X":
		_, _ = db.Exec("UPDATE users SET wins = wins + 1 WHERE username = $1", userX)
		_, _ = db.Exec("UPDATE users SET losses = losses + 1 WHERE username = $1", userO)
	case "O":
		_, _ = db.Exec("UPDATE users SET wins = wins + 1 WHERE username = $1", userO)
		_, _ = db.Exec("UPDATE users SET losses = losses + 1 WHERE username = $1", userX)
	case "DRAW":
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

	switch c {
	case r.PlayerX:
		r.RetryX = true
		r.PlayerO.safeSend(Message{Type: "RETRY_REQUESTED", Message: "Opponent wants a rematch!"})
	case r.PlayerO:
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