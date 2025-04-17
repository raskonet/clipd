package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second    // Time allowed to write a message to the peer.
	pongWait       = 60 * time.Second    // Time allowed to read the next pong message from the peer.
	pingPeriod     = (pongWait * 9) / 10 // Send pings to peer with this period. Must be less than pongWait.
	maxMessageSize = 512 * 1024        // Maximum message size allowed from peer.
)

// connectCmd attempts to establish a WebSocket connection.
// It returns a tea.Msg indicating the result (ConnectionStatusMsg).
func connectCmd(serverURL, apiKey, hostname string) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Attempting to connect to %s", serverURL)

		u, err := url.Parse(serverURL)
		if err != nil {
			return ErrorMsg{fmt.Errorf("parsing url: %w", err)}
		}

		q := u.Query()
		q.Set("apiKey", apiKey)
		q.Set("hostname", hostname)
		u.RawQuery = q.Encode()

		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Printf("Dial error: %v", err)
			// Return error status without connection details
			return ConnectionStatusMsg{Status: Disconnected, Err: fmt.Errorf("dial failed: %w", err)}
		}
		log.Println("WebSocket connected.")

		// Create context for managing background goroutines for this connection
		ctx, cancel := context.WithCancel(context.Background())

		// Return success message with connection details and cancel function
		return ConnectionStatusMsg{Status: Connected, Conn: conn, Cancel: cancel, Err: nil}
	}
}

// listenWebSocketCmd starts the read and ping loops for the WebSocket connection.
// It requires the Program instance to send messages back to the main Update loop.
func listenWebSocketCmd(ctx context.Context, conn *websocket.Conn, p *tea.Program) tea.Cmd {
	return func() tea.Msg {
		log.Println("Starting WebSocket listener...")
		conn.SetReadLimit(maxMessageSize)
		conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})

		// Goroutine for reading messages
		go func() {
			defer func() {
				log.Println("WebSocket read loop finished.")
				// Optionally notify main thread about disconnection here too
				// p.Send(ConnectionStatusMsg{Status: Disconnected}) // Be careful about race conditions
			}()
			for {
				select {
				case <-ctx.Done():
					log.Println("Read loop cancelled via context.")
					return // Exit goroutine
				default:
					messageType, message, err := conn.ReadMessage()
					if err != nil {
						if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
							log.Printf("Read error: %v", err)
							p.Send(ConnectionStatusMsg{Status: Disconnected, Err: fmt.Errorf("read error: %w", err)})
						} else {
							log.Printf("WebSocket closed normally or timed out.")
							p.Send(ConnectionStatusMsg{Status: Disconnected, Err: nil})
						}
						return // Exit goroutine on error or close
					}
					// Reset read deadline on successful read
					conn.SetReadDeadline(time.Now().Add(pongWait))

					if messageType == websocket.TextMessage {
						var msg BaseMessage
						if err := json.Unmarshal(message, &msg); err != nil {
							log.Printf("Unmarshal error: %v", err)
							p.Send(ErrorMsg{fmt.Errorf("unmarshal error: %w", err)})
							continue
						}
						// Send the parsed message to the main Update loop
						p.Send(ReceivedServerMsg{Msg: msg})

					} else if messageType == websocket.BinaryMessage {
						log.Printf("Received Binary Message (%d bytes) - Ignoring", len(message))
						// TODO: Handle binary messages (file chunks) - requires state and logic
						// Could send a specific tea.Msg for binary data if needed
					}
				}
			}
		}()

		// Goroutine for sending pings
		go func() {
			ticker := time.NewTicker(pingPeriod)
			defer func() {
				ticker.Stop()
				log.Println("WebSocket ping loop finished.")
			}()
			for {
				select {
				case <-ticker.C:
					conn.SetWriteDeadline(time.Now().Add(writeWait))
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						log.Printf("Ping error: %v", err)
						// Don't necessarily disconnect here, read loop will detect closure
						return // Exit ping loop
					}
				case <-ctx.Done():
					log.Println("Ping loop cancelled via context.")
					return // Exit goroutine
				}
			}
		}()

		return nil // Indicate listener setup started (actual status comes via p.Send)
	}
}

// sendWebsocketMessageCmd sends a JSON message over the WebSocket.
func sendWebsocketMessageCmd(conn *websocket.Conn, message BaseMessage) tea.Cmd {
	return func() tea.Msg {
		if conn == nil {
			return ErrorMsg{Err: fmt.Errorf("cannot send: not connected")}
		}

		msgBytes, err := json.Marshal(message)
		if err != nil {
			return ErrorMsg{Err: fmt.Errorf("marshalling ws message: %w", err)}
		}

		conn.SetWriteDeadline(time.Now().Add(writeWait))
		err = conn.WriteMessage(websocket.TextMessage, msgBytes)
		conn.SetWriteDeadline(time.Time{}) // Clear deadline immediately
		if err != nil {
			log.Printf("Websocket write error: %v", err)
			// Return error, might trigger disconnect logic in model
			return ErrorMsg{Err: fmt.Errorf("websocket write failed: %w", err)}
		}
		log.Printf("WS Sent: Type=%s", message.Type)
		return nil // Indicate success (no message needed back to Update)
	}
}

// sendWebsocketBinaryCmd sends a binary message (e.g., file chunk).
func sendWebsocketBinaryCmd(conn *websocket.Conn, data []byte) tea.Cmd {
	return func() tea.Msg {
		if conn == nil {
			return ErrorMsg{Err: fmt.Errorf("cannot send binary: not connected")}
		}

		conn.SetWriteDeadline(time.Now().Add(writeWait)) // Adjust deadline based on chunk size?
		err := conn.WriteMessage(websocket.BinaryMessage, data)
		conn.SetWriteDeadline(time.Time{})
		if err != nil {
			log.Printf("Websocket binary write error: %v", err)
			return ErrorMsg{Err: fmt.Errorf("websocket binary write failed: %w", err)}
		}
		log.Printf("WS Sent: Binary Data (%d bytes)", len(data))
		return nil
	}
}

// checkLocalClipboardCmd reads the local clipboard and sends a message if changed.
func checkLocalClipboardCmd(lastContent string) tea.Cmd {
	return func() tea.Msg {
		// Use the cross-platform clipboard library
		currentClip, err := clipboard.ReadAll()
		if err != nil {
			// Don't spam logs for transient errors, maybe log occasionally
			// log.Printf("Error reading local clipboard: %v", err)
			return LocalClipboardCheckedMsg{Changed: false, Err: err}
		}

		if currentClip != lastContent {
			return LocalClipboardCheckedMsg{Content: currentClip, Changed: true, Err: nil}
		}
		return LocalClipboardCheckedMsg{Changed: false, Err: nil} // No change
	}
}

// writeToClipboardCmd writes content to the local clipboard.
func writeToClipboardCmd(content string) tea.Cmd {
	return func() tea.Msg {
		err := clipboard.WriteAll(content)
		if err != nil {
			log.Printf("Error writing to local clipboard: %v", err)
			return ErrorMsg{fmt.Errorf("clipboard write failed: %w", err)}
		}
		return LogMsg("Local clipboard updated.") // Notify user via log
	}
}
