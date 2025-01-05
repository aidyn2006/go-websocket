package main

import (
	_ "fmt"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"sync"
)

// WebSocket upgrader to upgrade HTTP connection to WebSocket connection
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow connections from any origin
	},
}

// Client struct represents a single WebSocket client
type Client struct {
	conn *websocket.Conn
	send chan []byte
}

// Global clients map and mutex to synchronize access to clients
var clients = make(map[*Client]bool)
var mu sync.Mutex

// Broadcast message to all connected clients
func broadcastMessage(message []byte, sender *Client) {
	mu.Lock()
	defer mu.Unlock()
	// Iterate through all clients and send the message to each one
	for client := range clients {
		if client != sender {
			select {
			case client.send <- message:
			default:
				// If the channel is full, close the connection
				close(client.send)
				delete(clients, client)
			}
		}
	}
}

// WebSocket connection handler
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading connection:", err)
		return
	}
	client := &Client{conn: conn, send: make(chan []byte, 256)}

	// Add the client to the clients map
	mu.Lock()
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

		// Broadcast the message to all other clients
		broadcastMessage(msg, client)
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
