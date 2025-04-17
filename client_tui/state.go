package main

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket" // Needed for message type definition
)

// --- Connection State ---
type ConnectionState int

const (
	Connecting ConnectionState = iota
	Connected
	Disconnected
)

func (s ConnectionState) String() string {
	return [...]string{"Connecting", "Connected", "Disconnected"}[s]
}

// --- Server Message Structs (mirrored for client use) ---
// These should match the structs used by the server

type ClientInfo struct {
	ID       string `json:"id"`
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
	SourceID string `json:"sourceId"` // ID of the client who offered
}

// --- Bubbletea Messages ---
// Messages passed between goroutines and Model.Update

type ConnectionStatusMsg struct {
	Status ConnectionState
	Err    error
	Conn   *websocket.Conn // Pass the established connection
	Cancel func()          // Function to cancel background context
}
type ReceivedServerMsg struct{ Msg BaseMessage } // Generic message from server
type LocalClipboardCheckedMsg struct {
	Content string
	Changed bool
	Err     error
}
type ErrorMsg struct{ Err error }
type LogMsg string // Simple message to add to log view

// --- Keybindings ---
type keyMap struct {
	Quit        key.Binding
	ToggleSync  key.Binding
	FocusNext   key.Binding // Example: Tab
	FocusPrev   key.Binding // Example: Shift+Tab
	AcceptFile  key.Binding // Example: 'a'
	RejectFile  key.Binding // Example: 'r'
	InitiateXfer key.Binding // Example: 'x' (needs device list focus)
	// Add keys for list navigation (handled by list.Model)
}

func defaultKeyMap() keyMap {
	return keyMap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c", "q"),
			key.WithHelp("q/ctrl+c", "quit"),
		),
		ToggleSync: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "toggle sync"),
		),
		FocusNext: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next panel"),
		),
		FocusPrev: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev panel"),
		),
		AcceptFile: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "accept file"),
		),
		RejectFile: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reject file"),
		),
		InitiateXfer: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "initiate transfer (on device)"),
		),
	}
}

// --- List Items ---

// historyItem implements list.Item for clipboard history
type historyItem string

func (h historyItem) FilterValue() string { return string(h) }
func (h historyItem) Title() string       { return string(h) }
func (h historyItem) Description() string { return "" } // No description needed

// deviceItem implements list.Item for connected devices
type deviceItem ClientInfo // Use the ClientInfo struct

func (d deviceItem) FilterValue() string { return d.Hostname }
func (d deviceItem) Title() string       { return d.Hostname }
func (d deviceItem) Description() string { return fmt.Sprintf("ID: %s", d.ID) }

// --- File Transfer State (placeholder) ---
type fileTransferState struct {
	IsOffering    bool
	IsReceiving   bool
	OfferDetails  *FileOfferData
	OfferingTo    string // Client ID
	ReceivingFrom string // Client ID
	Filename      string
	Progress      float64 // 0.0 to 1.0
	// Add more state as needed (file handles, chunk tracking etc)
}
