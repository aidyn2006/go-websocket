package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

// WebSocket upgrader to upgrade HTTP connection to WebSocket connection
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow connections from any origin
	},
}

// Client struct represents a single WebSocket client
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	username string
	userID   int // Добавьте это поле для хранения идентификатора пользователя

}

// Global clients map, admin client, and mutex to synchronize access to clients
var clients = make(map[*Client]bool)
var adminClient *Client
var mu sync.Mutex

// Store messages for each user
var userMessages = make(map[string][]string) // Store history of messages for each user

// Database connection
var db *sql.DB

// Initialize database connection
func initDB() {
	var err error
	// Connect to PostgreSQL
	connStr := "user=postgres password=Na260206 dbname=socket sslmode=disable"
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		log.Fatal("Failed to create users table:", err)
	}

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS messages (
		id SERIAL PRIMARY KEY,
		sender_id INTEGER REFERENCES users(id),
		consumer_id INTEGER REFERENCES users(id),
		message TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		log.Fatal("Failed to create messages table:", err)
	}
}

// Save client to the database
func saveClientToDB(username string) error {
	_, err := db.Exec("INSERT INTO users (username) VALUES ($1) ON CONFLICT (username) DO NOTHING", username)
	if err != nil {
		return fmt.Errorf("failed to save client to DB: %w", err)
	}
	return nil
}

// Save message to the database
func saveMessageToDB(senderID, consumerID int, message string) error {
	_, err := db.Exec("INSERT INTO messages (sender_id, consumer_id, message) VALUES ($1, $2, $3)", senderID, consumerID, message)
	if err != nil {
		log.Println("Error saving message to DB:", err) // Добавлен лог ошибки
		return fmt.Errorf("failed to save message to DB: %w", err)
	}
	log.Printf("Message saved from user %d to user %d: %s", senderID, consumerID, message) // Лог успешного сохранения
	return nil
}

// Fetch message history for a specific user
func getMessageHistory(userID int) ([]string, error) {
	rows, err := db.Query(`
		SELECT m.message
		FROM messages m
		WHERE m.sender_id = $1 OR m.consumer_id = $1
		ORDER BY m.created_at
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch message history: %w", err)
	}
	defer rows.Close()

	var messages []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		messages = append(messages, message)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return messages, nil
}

// Broadcast message to a specific user
func broadcastToUser(message []byte, targetUsername string) {
	mu.Lock()
	defer mu.Unlock()

	// Send the message to the target user
	for client := range clients {
		if client.username == targetUsername {
			select {
			case client.send <- message:
			default:
				close(client.send)
				delete(clients, client)
			}
			break
		}
	}
}

// Broadcast message to all clients except the sender
func broadcastToAll(message []byte, sender *Client) {
	mu.Lock()
	defer mu.Unlock()

	for client := range clients {
		if client != sender {
			select {
			case client.send <- message:
			default:
				close(client.send)
				delete(clients, client)
			}
		}
	}
}

// WebSocket connection handler
// WebSocket connection handler
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP connection to WebSocket connection
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading connection:", err)
		return
	}
	defer conn.Close()

	// Read the username sent by the client (first message)
	_, usernameBytes, err := conn.ReadMessage()
	if err != nil {
		log.Println("Error reading username:", err)
		return
	}
	username := string(usernameBytes)

	// Save user to DB
	err = saveClientToDB(username)
	if err != nil {
		log.Println("Error saving client to DB:", err)
		return
	}

	// Get user ID from DB
	var userID int
	err = db.QueryRow("SELECT id FROM users WHERE username = $1", username).Scan(&userID)
	if err != nil {
		log.Println("Error getting user ID:", err)
		return
	}

	client := &Client{conn: conn, send: make(chan []byte, 256), username: username, userID: userID}

	// Check if this is the admin client
	mu.Lock()
	if username == "admin" {
		adminClient = client
	}
	clients[client] = true
	mu.Unlock()

	// Send the message history to the client if it exists
	messages, err := getMessageHistory(userID)
	if err != nil {
		log.Println("Error getting message history:", err)
	} else {
		for _, msg := range messages {
			client.send <- []byte(msg) // Send the history
		}
	}

	defer func() {
		mu.Lock()
		delete(clients, client)
		mu.Unlock()
		conn.Close()
	}()

	// Start a goroutine to send messages to the client
	go func() {
		for msg := range client.send {
			err := conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				log.Println("Error writing message:", err)
				break
			}
		}
	}()

	// Handle messages from the WebSocket client
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Error reading message:", err)
			break
		}

		// If the client is not admin, send the message only to the admin
		if client != adminClient {
			// Store message in history
			userMessages[client.username] = append(userMessages[client.username], string(msg))

			// Save the message to the database
			err := saveMessageToDB(userID, adminClient.userID, string(msg))
			if err != nil {
				log.Println("Error saving message to DB:", err)
			}

			// Send the message to the admin
			broadcastToUser(msg, "admin")
			select {
			case client.send <- msg: // Send message back to the user
			default:
				close(client.send)
				delete(clients, client)
			}
		} else {
			// Admin can send messages to specific users or broadcast
			parts := strings.SplitN(string(msg), ":", 2)
			if len(parts) == 2 {
				targetUsername := parts[1]
				messageContent := parts[0]

				// Get the target user's ID from the DB
				var targetUserID int
				err := db.QueryRow("SELECT id FROM users WHERE username = $1", targetUsername).Scan(&targetUserID)
				if err != nil {
					log.Println("Error getting target user's ID:", err)
					continue
				}

				// Save the message to the database
				err = saveMessageToDB(adminClient.userID, targetUserID, messageContent)
				if err != nil {
					log.Println("Error saving message to DB:", err)
					continue
				}

				// Send message to the target user
				broadcastToUser([]byte(messageContent), targetUsername)
			} else {
				// Admin can broadcast message to all users
				broadcastToAll(msg, client)
			}
		}
	}
}

func main() {
	// Initialize database
	initDB()

	// Serve static files like HTML, JS, etc.
	http.Handle("/", http.FileServer(http.Dir("./public")))

	// WebSocket route
	http.HandleFunc("/ws", handleWebSocket)

	// Start the server
	log.Println("Starting WebSocket server on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe failed:", err)
	}
}
