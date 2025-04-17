package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const maxHistorySize = 20

type ClientInfo struct {
	ID       string `json:"id"`
	Conn     *websocket.Conn `json:"-"`
	Hostname string `json:"hostname"`
}

type BaseMessage struct {
	Type     string      `json:"type"`
	Data     interface{} `json:"data"`
	SenderID string      `json:"senderId,omitempty"`
}

type ClipboardUpdateData struct {
	Content string `json:"content"`
}

type ClipboardHistoryData struct {
	History []string `json:"history"`
}

type DeviceListData struct {
	Devices []ClientInfo `json:"devices"`
}

type FileOfferData struct {
	Filename string `json:"filename"`
	Filesize int64  `json:"filesize"`
	TargetID string `json:"targetId,omitempty"`
}

type FileAckData struct {
	Filename string `json:"filename"`
	Allow    bool   `json:"allow"`
	SourceID string `json:"sourceId"`
}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	clients          = make(map[string]*ClientInfo)
	broadcast        = make(chan BaseMessage)
	register         = make(chan *ClientInfo)
	unregister       = make(chan *ClientInfo)
	mutex            = &sync.RWMutex{}
	currentClip      = ""
	clipboardLock    = &sync.RWMutex{}
	apiKey           string
	clipboardHistory []string
	historyMutex     sync.Mutex
)

func loadEnv() {
	err := godotenv.Load("../.env")
	if err != nil {
		log.Println("Warning: Could not load .env file.")
	}
	apiKey = os.Getenv("CLIPBOARD_API_KEY")
	if apiKey == "" {
		log.Fatal("Error: CLIPBOARD_API_KEY not set")
	}
}

func runHub() {
	for {
		select {
		case client := <-register:
			mutex.Lock()
			clients[client.ID] = client
			log.Printf("Client registered: %s (%s)", client.ID, client.Hostname)
			mutex.Unlock()
			broadcastDeviceListUpdate()

		case client := <-unregister:
			mutex.Lock()
			if existingClient, ok := clients[client.ID]; ok {
				// Ensure we are closing the correct connection if client object was recreated
				if existingClient.Conn == client.Conn {
					delete(clients, client.ID)
					close(client.Conn) // Use non-blocking close helper?
					log.Printf("Client unregistered: %s (%s)", client.ID, client.Hostname)
				}
			}
			mutex.Unlock()
			broadcastDeviceListUpdate()

		case message := <-broadcast:
			mutex.RLock()
			activeClients := make([]*ClientInfo, 0, len(clients))
			for _, client := range clients {
				activeClients = append(activeClients, client)
			}
			mutex.RUnlock() // Release lock before potentially slow network writes

			msgBytes, err := json.Marshal(message)
			if err != nil {
				log.Printf("Error marshalling broadcast message: %v", err)
				continue
			}

			for _, client := range activeClients {
				// Skip sender for certain types
				if message.Type == "clipboard_update" && client.ID == message.SenderID {
					continue
				}

				// Handle targeted messages
				targetted := false
				switch data := message.Data.(type) {
				case FileAckData:
					if message.Type == "file_ack" && client.ID != data.SourceID {
						targetted = true
					}
				case FileOfferData:
					if message.Type == "file_offer" {
						if data.TargetID != "" && client.ID != data.TargetID { // Skip if targetted and not the target
							targetted = true
						}
						if client.ID == message.SenderID { // Don't send offer to self
							targetted = true
						}
					}
				}
				if targetted {
					continue
				}

				err := writeToClient(client, websocket.TextMessage, msgBytes)
				if err != nil {
					log.Printf("Write error to client %s: %v", client.ID, err)
					// Trigger unregistration for this client
					// Use a non-blocking send to avoid deadlocking the hub
					go func(c *ClientInfo) {
						select {
						case unregister <- c:
						default: // Hub might be busy, client might already be unregistering
							log.Printf("Unregister channel full or blocked for client %s", c.ID)
						}
					}(client)
				}
			}
		}
	}
}

// Helper to prevent blocking writes from locking up the hub or read loops
func writeToClient(client *ClientInfo, messageType int, data []byte) error {
	client.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)) // Add a deadline
	err := client.Conn.WriteMessage(messageType, data)
	client.Conn.SetWriteDeadline(time.Time{}) // Clear deadline
	return err
}


func broadcastDeviceListUpdate() {
	mutex.RLock()
	deviceList := make([]ClientInfo, 0, len(clients))
	for _, c := range clients {
		// Only include ID and Hostname in broadcast, not the Conn
		deviceList = append(deviceList, ClientInfo{ID: c.ID, Hostname: c.Hostname})
	}
	mutex.RUnlock()

	message := BaseMessage{
		Type: "device_list",
		Data: DeviceListData{Devices: deviceList},
	}
	// Send non-blockingly to broadcast channel to avoid deadlock if hub is busy
	select {
	case broadcast <- message:
	default:
		log.Println("Broadcast channel full when sending device list update.")
	}
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	queryApiKey := r.URL.Query().Get("apiKey")
	if queryApiKey != apiKey {
		log.Printf("Auth failed: Invalid API Key from %s", r.RemoteAddr)
		http.Error(w, "Forbidden: Invalid API Key", http.StatusForbidden)
		return
	}

	hostname := r.URL.Query().Get("hostname")
	if hostname == "" {
		hostname = "Unknown"
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}

	client := &ClientInfo{
		ID:       uuid.NewString(),
		Conn:     ws,
		Hostname: hostname,
	}
	register <- client // Register with the hub

	// Send initial state directly (hub handles subsequent broadcasts)
	clipboardLock.RLock()
	current := currentClip
	clipboardLock.RUnlock()
	if current != "" {
		msg := BaseMessage{Type: "clipboard_update", Data: ClipboardUpdateData{Content: current}}
		msgBytes, _ := json.Marshal(msg)
		writeToClient(client, websocket.TextMessage, msgBytes) // Use helper
	}

	historyMutex.Lock()
	historyCopy := make([]string, len(clipboardHistory))
	copy(historyCopy, clipboardHistory)
	historyMutex.Unlock()
	if len(historyCopy) > 0 {
		msg := BaseMessage{Type: "clipboard_history", Data: ClipboardHistoryData{History: historyCopy}}
		msgBytes, _ := json.Marshal(msg)
		writeToClient(client, websocket.TextMessage, msgBytes) // Use helper
	}

	// Start the read loop for this client
	readLoop(client)

	// When readLoop returns, trigger unregistration
	unregister <- client
}

func readLoop(client *ClientInfo) {
	defer func() {
		// This runs when the loop exits for any reason (error, normal close)
		log.Printf("Exiting read loop for %s (%s)", client.ID, client.Hostname)
	}()
	// Configure connection properties
	client.Conn.SetReadLimit(512 * 1024) // Set max message size (adjust as needed)
	client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second)) // Pong timeout
	client.Conn.SetPongHandler(func(string) error {
		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	// Add Ping handler? Maybe server should ping clients periodically.

	for {
		messageType, p, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("Read error from %s (%s): %v", client.ID, client.Hostname, err)
			} else {
				log.Printf("Client %s (%s) disconnected normally or timed out.", client.ID, client.Hostname)
			}
			break // Exit loop on any error or close
		}
		// Reset read deadline after successful read
		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		if messageType == websocket.TextMessage {
			var msg BaseMessage
			if err := json.Unmarshal(p, &msg); err != nil {
				log.Printf("Unmarshal error from %s: %v", client.ID, err)
				continue
			}

			msg.SenderID = client.ID // Inject sender ID

			switch msg.Type {
			case "clipboard_update":
				var data ClipboardUpdateData
				if err := RemarshalData(msg.Data, &data); err == nil {
					clipboardLock.Lock()
					if currentClip != data.Content {
						currentClip = data.Content
						historyMutex.Lock()
						clipboardHistory = append([]string{currentClip}, clipboardHistory...)
						if len(clipboardHistory) > maxHistorySize {
							clipboardHistory = clipboardHistory[:maxHistorySize]
						}
						historyMutex.Unlock()

						broadcastMsg := BaseMessage{Type: "clipboard_update", Data: data, SenderID: client.ID}
						broadcast <- broadcastMsg // Let hub handle broadcast
					}
					clipboardLock.Unlock()
				} else {
					log.Printf("Error unmarshalling clipboard_update data from %s: %v", client.ID, err)
				}

			case "request_devices":
				mutex.RLock()
				deviceList := make([]ClientInfo, 0, len(clients))
				for _, c := range clients {
					deviceList = append(deviceList, ClientInfo{ID: c.ID, Hostname: c.Hostname})
				}
				mutex.RUnlock()
				response := BaseMessage{Type: "device_list", Data: DeviceListData{Devices: deviceList}}
				respBytes, _ := json.Marshal(response)
				writeToClient(client, websocket.TextMessage, respBytes) // Use helper

			case "file_offer":
				var data FileOfferData
				if err := RemarshalData(msg.Data, &data); err == nil {
					log.Printf("Received file offer '%s' from %s", data.Filename, client.Hostname)
					broadcast <- msg // Let hub handle routing
				} else {
					log.Printf("Error unmarshalling file_offer data from %s: %v", client.ID, err)
				}

			case "file_ack":
				var data FileAckData
				if err := RemarshalData(msg.Data, &data); err == nil {
					log.Printf("Received file ack '%v' for '%s' from %s", data.Allow, data.Filename, client.Hostname)
					broadcast <- msg // Let hub handle routing
				} else {
					log.Printf("Error unmarshalling file_ack data from %s: %v", client.ID, err)
				}

			default:
				log.Printf("Received unknown message type '%s' from %s", msg.Type, client.Hostname)
			}

		} else if messageType == websocket.BinaryMessage {
			log.Printf("Received binary message from %s (%d bytes) - Potential file chunk (IGNORED)", client.ID, len(p))
			// TODO: Implement file chunk handling logic
		}
	}
}

// RemarshalData helper to decode nested data structures
func RemarshalData(data interface{}, target interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, target)
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	loadEnv()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	clipboardHistory = make([]string, 0, maxHistorySize)

	go runHub() // Start the central hub

	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/health", healthCheck)

	log.Println("HTTP server starting on", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
