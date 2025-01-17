package main

import (
	"database/sql"
	"encoding/json"
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
	userID   int // Add this field for storing user ID
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

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS admin_clients (
		client_id INTEGER REFERENCES users(id),
		PRIMARY KEY (client_id)
	)`)
	if err != nil {
		log.Fatal("Failed to create admin_clients table:", err)
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

// Get all user IDs from the admin_clients table and find their usernames
// Get all user IDs from the admin_clients table and find their usernames
func getAdminUsernames() {
	// Get all user IDs from admin_clients table
	rows, err := db.Query("SELECT client_id FROM admin_clients")
	if err != nil {
		log.Fatal("Error fetching user IDs from admin_clients table:", err)
	}
	defer rows.Close()

	var usernames []string
	// Iterate over each user ID and find the username
	for rows.Next() {
		var userID int
		if err := rows.Scan(&userID); err != nil {
			log.Fatal("Error scanning user ID:", err)
		}

		// Get username from users table based on the user ID
		var username string
		err := db.QueryRow("SELECT username FROM users WHERE id = $1", userID).Scan(&username)
		if err != nil {
			log.Printf("Error getting username for user ID %d: %v", userID, err)
			continue
		}

		usernames = append(usernames, username)
	}

	if err := rows.Err(); err != nil {
		log.Fatal("Error during rows iteration:", err)
	}

	// Broadcast the list of usernames to all clients
	for client := range clients {
		select {
		case client.send <- []byte("users:" + strings.Join(usernames, ",")):
		default:
			close(client.send)
			delete(clients, client)
		}
	}
}

// Save message to the database
func saveMessageToDB(senderID, consumerID int, message string) error {
	_, err := db.Exec("INSERT INTO messages (sender_id, consumer_id, message) VALUES ($1, $2, $3)", senderID, consumerID, message)
	if err != nil {
		log.Println("Error saving message to DB:", err)
		return fmt.Errorf("failed to save message to DB: %w", err)
	}
	log.Printf("Message saved from user %d to user %d: %s", senderID, consumerID, message)
	return nil
}

// Fetch message history for a specific user
// Fetch message history between sender and consumer (admin or user)
func getMessageHistory(senderID, consumerID int) ([]string, error) {
	rows, err := db.Query(`
		SELECT m.message
		FROM messages m
		WHERE (m.sender_id = $1 AND m.consumer_id = $2) OR (m.sender_id = $2 AND m.consumer_id = $1)
		ORDER BY m.created_at
	`, senderID, consumerID)
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

// Handler to send the list of users to the admin
func getUserList(w http.ResponseWriter, r *http.Request) {
	// Get all user IDs from admin_clients table
	rows, err := db.Query("SELECT client_id FROM admin_clients")
	if err != nil {
		http.Error(w, "Error fetching user list", http.StatusInternalServerError)
		log.Println("Error fetching user list:", err)
		return
	}
	defer rows.Close()

	var usernames []string
	// Iterate over each user ID and find the username
	for rows.Next() {
		var userID int
		if err := rows.Scan(&userID); err != nil {
			log.Println("Error scanning user ID:", err)
			continue
		}

		// Get username from users table based on the user ID
		var username string
		err := db.QueryRow("SELECT username FROM users WHERE id = $1", userID).Scan(&username)
		if err != nil {
			log.Printf("Error getting username for user ID %d: %v", userID, err)
			continue
		}

		usernames = append(usernames, username)
	}

	if err := rows.Err(); err != nil {
		log.Fatal("Error during rows iteration:", err)
	}

	// Send the list of usernames back as a response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(usernames)
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
		// Save admin to the admin_clients table (if not already done)
		_, err := db.Exec(`
			INSERT INTO admin_clients (client_id)
			(SELECT id FROM users WHERE username = $1)
			ON CONFLICT (client_id) DO NOTHING
		`, username)
		if err != nil {
			log.Println("Error saving admin to admin_clients table:", err)
		}
	}
	clients[client] = true
	mu.Unlock()

	// Send the message history to the client if it exists
	messages, err := getMessageHistory(userID, adminClient.userID)
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
		// When a client (non-admin) sends a message, check if they are the admin and handle accordingly
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

			// Check if the user has sent a message to the admin for the first time
			_, err = db.Exec(`
        INSERT INTO admin_clients (client_id) 
        SELECT id FROM users WHERE username = $1
        AND NOT EXISTS (SELECT 1 FROM admin_clients WHERE client_id = id)
    `, client.username)
			if err != nil {
				log.Println("Error saving user to admin_clients table:", err)
			}

			// Send the message back to the user
			select {
			case client.send <- msg:
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
			} else if strings.HasPrefix(string(msg), "history:") {
				// If the message starts with "history:", fetch message history for the specified user
				targetUsername := strings.TrimPrefix(string(msg), "history:")

				var targetUserID int
				err := db.QueryRow("SELECT id FROM users WHERE username = $1", targetUsername).Scan(&targetUserID)
				if err != nil {
					log.Println("Error getting target user's ID:", err)
					continue
				}

				// Get message history for the admin from the specific user
				history, err := getMessageHistory(adminClient.userID, targetUserID)
				if err != nil {
					log.Println("Error fetching message history:", err)
					continue
				}

				// Send the history to the admin
				for _, msg := range history {
					select {
					case adminClient.send <- []byte(msg):
					default:
						close(adminClient.send)
						delete(clients, adminClient)
					}
				}
			} else {
				// Admin can broadcast message to all users
				broadcastToAll(msg, client)
			}
		}
	}
}

// Delete a user and all associated data (messages, admin_clients, etc.)
func deleteUser(w http.ResponseWriter, r *http.Request) {
	// Get the username from the URL path
	username := strings.TrimPrefix(r.URL.Path, "/delete-user/")

	// Check if the username is "admin" (admin cannot be deleted)
	if username == "admin" {
		http.Error(w, "Cannot delete the admin user", http.StatusForbidden)
		return
	}

	// Get the user ID from the database
	var userID int
	err := db.QueryRow("SELECT id FROM users WHERE username = $1", username).Scan(&userID)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		log.Println("Error getting user ID:", err)
		return
	}

	// Delete messages related to this user
	_, err = db.Exec("DELETE FROM messages WHERE sender_id = $1 OR consumer_id = $1", userID)
	if err != nil {
		http.Error(w, "Failed to delete messages", http.StatusInternalServerError)
		log.Println("Error deleting messages:", err)
		return
	}

	// Remove the user from the admin_clients table (if they are listed there)
	_, err = db.Exec("DELETE FROM admin_clients WHERE client_id = $1", userID)
	if err != nil {
		http.Error(w, "Failed to remove from admin_clients", http.StatusInternalServerError)
		log.Println("Error removing from admin_clients:", err)
		return
	}

	// Delete the user from the users table
	_, err = db.Exec("DELETE FROM users WHERE id = $1", userID)
	if err != nil {
		http.Error(w, "Failed to delete user", http.StatusInternalServerError)
		log.Println("Error deleting user:", err)
		return
	}

	// Send a success response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("User %s deleted successfully", username)))

	// Optionally, broadcast that the user has been deleted to all clients
	mu.Lock()
	for client := range clients {
		if client.username == username {
			// Close the client's connection
			close(client.send)
			delete(clients, client)
			break
		}
	}
	mu.Unlock()
}

func main() {
	// Initialize database
	initDB()

	// Fetch and display admin usernames on program start
	getAdminUsernames()

	// Serve static files like HTML, JS, etc.
	http.Handle("/", http.FileServer(http.Dir("./public")))

	// WebSocket route
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/get-users", getUserList)

	// Add new route for deleting a user
	http.HandleFunc("/delete-user/", deleteUser) // Dynamic route for deleting user by username

	// Start the server
	log.Println("Starting WebSocket server on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe failed:", err)
	}
}
