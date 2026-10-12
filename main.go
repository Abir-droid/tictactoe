package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

const (
	writeWait   = 10 * time.Second
	pongWait    = 60 * time.Second
	pingPeriod  = (pongWait * 9) / 10
	gracePeriod = 60 * time.Second
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
	Type     string    `json:"type"` // "JOIN_QUEUE", "LEAVE_QUEUE", "MOVE", "MATCH_FOUND", "GAME_STATE", "GAME_OVER", "RETRY_REQUESTED", "RESTART_GAME", "OPPONENT_LEFT", "OPPONENT_DISCONNECTED", "OPPONENT_RECONNECTED", "ACTIVE_GAME_AVAILABLE", "REJOIN_GAME", "REJOIN_STATE", "CHAT", "ERROR"
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
	ID        string
	PlayerX   *Client
	PlayerO   *Client
	UsernameX string
	UsernameO string
	Board     [9]string
	Turn      string
	GameOver  bool
	RetryX    bool
	RetryO    bool
	TimerX    *time.Timer
	TimerO    *time.Timer
	Mu        sync.Mutex
}

var (
	matchQueue    = make(chan *Client, 100)
	leaveQueue    = make(chan *Client, 100)
	activeRooms   = make(map[string]*GameRoom)
	activeRoomsMu sync.Mutex
	upgrader      = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func getActiveRoom(username string) *GameRoom {
	activeRoomsMu.Lock()
	defer activeRoomsMu.Unlock()
	return activeRooms[username]
}

func setActiveRoom(username string, room *GameRoom) {
	activeRoomsMu.Lock()
	defer activeRoomsMu.Unlock()
	if room == nil {
		delete(activeRooms, username)
	} else {
		activeRooms[username] = room
	}
}

func withPrefixHandler(next http.Handler, customPrefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		prefixes := []string{"/tictactoe"}
		if customPrefix != "" && customPrefix != "/" && customPrefix != "/tictactoe" {
			prefixes = append([]string{customPrefix}, prefixes...)
		}

		for _, p := range prefixes {
			p = strings.TrimRight(p, "/")
			if p == "" || p == "/" {
				continue
			}

			// If the request path is exactly the prefix without trailing slash,
			// redirect to trailing slash so relative paths (CSS/JS) resolve correctly.
			if path == p {
				target := p + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}

			// If the request starts with prefix + "/"
			if strings.HasPrefix(path, p+"/") {
				r2 := new(http.Request)
				*r2 = *r
				r2URL := new(url.URL)
				*r2URL = *r.URL
				r2URL.Path = strings.TrimPrefix(path, p)
				if r2URL.Path == "" || !strings.HasPrefix(r2URL.Path, "/") {
					r2URL.Path = "/" + r2URL.Path
				}
				if r.URL.RawPath != "" {
					r2URL.RawPath = strings.TrimPrefix(r.URL.RawPath, p)
					if r2URL.RawPath != "" && !strings.HasPrefix(r2URL.RawPath, "/") {
						r2URL.RawPath = "/" + r2URL.RawPath
					}
				}
				r2.URL = r2URL
				r2.RequestURI = r2URL.RequestURI()
				next.ServeHTTP(w, r2)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	initDB()
	go matchmaker()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/leaderboard", handleLeaderboard)
	mux.HandleFunc("/api/profile", handleProfile)
	mux.HandleFunc("/ws", handleWebSocket)
	mux.Handle("/", http.FileServer(http.Dir("./public")))

	appRoot := strings.TrimRight(os.Getenv("APPLICATION_ROOT"), "/")
	if appRoot == "" {
		appRoot = strings.TrimRight(os.Getenv("BASE_PATH"), "/")
	}

	handler := withPrefixHandler(mux, appRoot)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 Game server running on port %s (prefix: %s)\n", port, appRoot)
	log.Fatal(http.ListenAndServe(":"+port, handler))
}

func fixPostgresConnStr(connStr string) string {
	connStr = strings.TrimSpace(connStr)
	if connStr == "" {
		return ""
	}

	// If sslmode is not explicitly set, default to sslmode=disable
	if !strings.Contains(connStr, "sslmode=") {
		if strings.Contains(connStr, "?") {
			connStr += "&sslmode=disable"
		} else {
			connStr += "?sslmode=disable"
		}
	}
	return connStr
}

func initDB() {
	rawConnStr := os.Getenv("DATABASE_URL")
	if rawConnStr == "" {
		log.Println("⚠️ DATABASE_URL not set, running without database persistence.")
		return
	}

	connStr := fixPostgresConnStr(rawConnStr)

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Printf("❌ Failed to open PostgreSQL driver: %v\n", err)
		return
	}

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		if strings.Contains(err.Error(), "SSL is not enabled") {
			log.Println("⚠️ Server has SSL disabled. Switching to sslmode=disable and reconnecting...")
			db.Close()
			connStr = strings.ReplaceAll(connStr, "sslmode=require", "sslmode=disable")
			connStr = strings.ReplaceAll(connStr, "sslmode=verify-full", "sslmode=disable")
			connStr = strings.ReplaceAll(connStr, "sslmode=verify-ca", "sslmode=disable")
			if !strings.Contains(connStr, "sslmode=disable") {
				if strings.Contains(connStr, "?") {
					connStr += "&sslmode=disable"
				} else {
					connStr += "?sslmode=disable"
				}
			}
			db, err = sql.Open("postgres", connStr)
			if err == nil {
				if err = db.Ping(); err == nil {
					fmt.Println("✅ Connected to PostgreSQL database successfully with sslmode=disable!")
					setupTable()
					return
				}
			}
		}
		log.Printf("⚠️ PostgreSQL ping failed: %v\n🔄 Will retry database connection in background...\n", err)
		go retryDBConnection(connStr)
		return
	}

	setupTable()
}

func setupTable() {
	if db == nil {
		return
	}
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
	_, err := db.Exec(tableQuery)
	if err != nil {
		log.Printf("⚠️ Failed to create users table: %v\n", err)
	} else {
		fmt.Println("✅ Connected to PostgreSQL database and initialized users table successfully!")
	}
}

func retryDBConnection(connStr string) {
	for i := 1; ; i++ {
		time.Sleep(5 * time.Second)
		if db == nil {
			return
		}
		if err := db.Ping(); err == nil {
			fmt.Println("✅ Connected to PostgreSQL database on background retry!")
			setupTable()
			return
		} else {
			if strings.Contains(err.Error(), "SSL is not enabled") {
				log.Println("🔄 Detected SSL not enabled on PostgreSQL. Reconnecting with sslmode=disable...")
				connStr = strings.ReplaceAll(connStr, "sslmode=require", "sslmode=disable")
				connStr = strings.ReplaceAll(connStr, "sslmode=verify-full", "sslmode=disable")
				connStr = strings.ReplaceAll(connStr, "sslmode=verify-ca", "sslmode=disable")
				if !strings.Contains(connStr, "sslmode=disable") {
					if strings.Contains(connStr, "?") {
						connStr += "&sslmode=disable"
					} else {
						connStr += "?sslmode=disable"
					}
				}
				db.Close()
				var openErr error
				db, openErr = sql.Open("postgres", connStr)
				if openErr == nil {
					if pingErr := db.Ping(); pingErr == nil {
						fmt.Println("✅ Connected to PostgreSQL database successfully with sslmode=disable!")
						setupTable()
						return
					}
				}
			}
			log.Printf("⚠️ PostgreSQL connection retry #%d failed: %v\n", i, err)
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	dbStatus := "disconnected"
	if db != nil {
		var dummy int
		err := db.QueryRow("SELECT 1").Scan(&dummy)
		if err == nil {
			dbStatus = "connected"
		} else {
			dbStatus = fmt.Sprintf("error: %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "OK",
		"database": dbStatus,
	})
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if db == nil {
		http.Error(w, "Database is currently unavailable", http.StatusServiceUnavailable)
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
		http.Error(w, "Username already exists or database error", http.StatusConflict)
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

	if db == nil {
		http.Error(w, "Database is currently unavailable", http.StatusServiceUnavailable)
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

	if db == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]LeaderboardEntry{})
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

	if db == nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
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

	if db != nil {
		var exists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", username).Scan(&exists)
		if err != nil || !exists {
			http.Error(w, "Unauthorized player", http.StatusUnauthorized)
			return
		}
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

	room := getActiveRoom(username)
	if room != nil {
		room.Mu.Lock()
		if !room.GameOver {
			var oppName string
			if username == room.UsernameX {
				oppName = room.UsernameO
			} else {
				oppName = room.UsernameX
			}
			client.safeSend(Message{
				Type:     "ACTIVE_GAME_AVAILABLE",
				Opponent: oppName,
			})
		}
		room.Mu.Unlock()
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
		ID:        fmt.Sprintf("%s_vs_%s", p1.Username, p2.Username),
		PlayerX:   p1,
		PlayerO:   p2,
		UsernameX: p1.Username,
		UsernameO: p2.Username,
		Turn:      "X",
	}

	p1.Room = room
	p1.Symbol = "X"

	p2.Room = room
	p2.Symbol = "O"

	setActiveRoom(p1.Username, room)
	setActiveRoom(p2.Username, room)

	p1.safeSend(Message{Type: "MATCH_FOUND", Symbol: "X", Turn: "X", Opponent: p2.Username})
	p2.safeSend(Message{Type: "MATCH_FOUND", Symbol: "O", Turn: "X", Opponent: p1.Username})
}

func (c *Client) readPump() {
	defer func() {
		leaveQueue <- c
		c.handleDisconnect()
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
			c.leaveCurrentRoomImmediately()
			matchQueue <- c

		case "LEAVE_QUEUE":
			c.leaveCurrentRoomImmediately()
			leaveQueue <- c

		case "REJOIN_GAME":
			c.handleRejoin()

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

func (c *Client) handleDisconnect() {
	if c.Room == nil {
		return
	}

	r := c.Room
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.GameOver {
		setActiveRoom(r.UsernameX, nil)
		setActiveRoom(r.UsernameO, nil)
		c.Room = nil
		return
	}

	var opponent *Client
	if c.Username == r.UsernameX {
		r.PlayerX = nil
		opponent = r.PlayerO
		if r.TimerX != nil {
			r.TimerX.Stop()
		}
		r.TimerX = time.AfterFunc(gracePeriod, func() {
			r.expireRoom(r.UsernameX)
		})
	} else if c.Username == r.UsernameO {
		r.PlayerO = nil
		opponent = r.PlayerX
		if r.TimerO != nil {
			r.TimerO.Stop()
		}
		r.TimerO = time.AfterFunc(gracePeriod, func() {
			r.expireRoom(r.UsernameO)
		})
	}

	if opponent != nil {
		opponent.safeSend(Message{
			Type:    "OPPONENT_DISCONNECTED",
			Message: fmt.Sprintf("%s disconnected. Waiting 60s for reconnection...", c.Username),
		})
	}

	c.Room = nil
}

func (r *GameRoom) expireRoom(disconnectedUser string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.GameOver {
		return
	}

	r.GameOver = true
	var remaining *Client
	if disconnectedUser == r.UsernameX {
		remaining = r.PlayerO
	} else {
		remaining = r.PlayerX
	}

	if remaining != nil {
		remaining.safeSend(Message{
			Type:    "OPPONENT_LEFT",
			Message: fmt.Sprintf("%s failed to reconnect within 1 minute.", disconnectedUser),
		})
	}

	setActiveRoom(r.UsernameX, nil)
	setActiveRoom(r.UsernameO, nil)
}

func (c *Client) handleRejoin() {
	room := getActiveRoom(c.Username)
	if room == nil {
		c.safeSend(Message{Type: "ERROR", Error: "No active game to rejoin."})
		return
	}

	room.Mu.Lock()
	defer room.Mu.Unlock()

	if room.GameOver {
		c.safeSend(Message{Type: "ERROR", Error: "The game has already ended."})
		setActiveRoom(c.Username, nil)
		return
	}

	var opp *Client
	var mySym string
	var oppName string

	if c.Username == room.UsernameX {
		room.PlayerX = c
		if room.TimerX != nil {
			room.TimerX.Stop()
			room.TimerX = nil
		}
		opp = room.PlayerO
		mySym = "X"
		oppName = room.UsernameO
	} else if c.Username == room.UsernameO {
		room.PlayerO = c
		if room.TimerO != nil {
			room.TimerO.Stop()
			room.TimerO = nil
		}
		opp = room.PlayerX
		mySym = "O"
		oppName = room.UsernameX
	}

	c.Room = room
	c.Symbol = mySym

	c.safeSend(Message{
		Type:     "REJOIN_STATE",
		Symbol:   mySym,
		Turn:     room.Turn,
		Board:    room.Board,
		Opponent: oppName,
	})

	if opp != nil {
		opp.safeSend(Message{
			Type:    "OPPONENT_RECONNECTED",
			Message: fmt.Sprintf("%s reconnected!", c.Username),
		})
		opp.safeSend(Message{
			Type:  "GAME_STATE",
			Board: room.Board,
			Turn:  room.Turn,
		})
	}
}

func (c *Client) leaveCurrentRoomImmediately() {
	if c.Room == nil {
		return
	}

	r := c.Room
	r.Mu.Lock()
	defer r.Mu.Unlock()

	r.GameOver = true

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

	setActiveRoom(r.UsernameX, nil)
	setActiveRoom(r.UsernameO, nil)
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
		if r.PlayerX != nil {
			r.PlayerX.safeSend(stateMsg)
		}
		if r.PlayerO != nil {
			r.PlayerO.safeSend(stateMsg)
		}

		go saveGameResult(r.UsernameX, r.UsernameO, winner)
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
	if r.PlayerX != nil {
		r.PlayerX.safeSend(broadcastMsg)
	}
	if r.PlayerO != nil {
		r.PlayerO.safeSend(broadcastMsg)
	}
}

func (r *GameRoom) handleChat(c *Client, text string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	chatMsg := Message{
		Type:    "CHAT",
		Message: text,
		Sender:  c.Username,
	}

	if r.PlayerX != nil {
		r.PlayerX.safeSend(chatMsg)
	}
	if r.PlayerO != nil {
		r.PlayerO.safeSend(chatMsg)
	}
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
		if r.PlayerO != nil {
			r.PlayerO.safeSend(Message{Type: "RETRY_REQUESTED", Message: "Opponent wants a rematch!"})
		}
	case r.PlayerO:
		r.RetryO = true
		if r.PlayerX != nil {
			r.PlayerX.safeSend(Message{Type: "RETRY_REQUESTED", Message: "Opponent wants a rematch!"})
		}
	}

	if r.RetryX && r.RetryO {
		r.Board = [9]string{}
		r.GameOver = false
		r.RetryX = false
		r.RetryO = false

		r.PlayerX, r.PlayerO = r.PlayerO, r.PlayerX
		r.UsernameX, r.UsernameO = r.UsernameO, r.UsernameX
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