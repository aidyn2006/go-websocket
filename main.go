package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
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
}

// Global clients map, admin client, and mutex to synchronize access to clients
var clients = make(map[*Client]bool)
var adminClient *Client
var mu sync.Mutex

// Broadcast message to the admin client only
func broadcastToAdmin(message []byte, sender *Client) {
	mu.Lock()
	defer mu.Unlock()
	if adminClient != nil && adminClient != sender {
		select {
		case adminClient.send <- message:
		default:
			close(adminClient.send)
			delete(clients, adminClient)
			adminClient = nil
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

	client := &Client{conn: conn, send: make(chan []byte, 256), username: username}

	// Check if this is the admin client
	mu.Lock()
	if username == "admin" {
		adminClient = client
	}
	clients[client] = true
	mu.Unlock()

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

		// If the client is not admin, send the message only to the admin and the client
		if client != adminClient {
			broadcastToAdmin(msg, client)
			select {
			case client.send <- msg: // Send message back to the client
			default:
				close(client.send)
				delete(clients, client)
			}
		} else {
			// Admin can broadcast to all clients
			broadcastToAll(msg, client)
		}
	}
}

func main() {
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
