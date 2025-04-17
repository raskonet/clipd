package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
)

type FocusablePane int

const maxHistorySize=20

const (
	HistoryPane FocusablePane = iota
	DevicesPane
	LogPane
	NumPanes // Keep last
)

type Model struct {
	// Config
	serverURL string
	apiKey    string
	hostname  string

	// UI Components
	spinner    spinner.Model
	deviceList list.Model
	histList   list.Model
	logView    viewport.Model
	help       help.Model
	keys       keyMap

	// State
	connectedState ConnectionState
	syncEnabled    bool
	lastError      error
	logMessages    []string
	wsConn         *websocket.Conn
	wsCtxCancel    context.CancelFunc // Function to cancel WS goroutines context
	lastSentClip   string
	lastRcvdClip   string
	focus          FocusablePane
	programRef     *tea.Program // Reference to program needed for sending messages from cmds

	// File Transfer State
	incomingFileOffer *FileOfferData
	offeringClientID  string // ID of client who sent the offer
	devicesMap        map[string]string // Map ID to hostname for lookup

	// Dimensions
	width, height int
	ready         bool // Flag to indicate if UI is ready (size known)
}

func NewModel(serverURL, apiKey, hostname string) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(special)

	histList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	histList.Title = "Clipboard History"
	histList.Styles.Title = listTitleStyle
	histList.SetShowHelp(false) // Use main help

	deviceList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	deviceList.Title = "Connected Devices"
	deviceList.Styles.Title = listTitleStyle
	deviceList.SetShowHelp(false) // Use main help

	logView := viewport.New(0, 0) // Size set later
	logView.SetContent("Initializing logs...")

	keys := defaultKeyMap()
	hlp := help.New()
	hlp.ShowAll = false // Show only short help

	m := Model{
		serverURL:      serverURL,
		apiKey:         apiKey,
		hostname:       hostname,
		spinner:        s,
		deviceList:     deviceList,
		histList:       histList,
		logView:        logView,
		help:           hlp,
		keys:           keys,
		connectedState: Disconnected, // Start disconnected
		syncEnabled:    true,
		focus:          HistoryPane,
		logMessages:    []string{"Initializing..."},
		devicesMap:     make(map[string]string),
	}
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,                 // Start spinner animation
		connectCmd(m.serverURL, m.apiKey, m.hostname), // Initiate connection attempt
	)
}


func RemarshalData(data interface{}, target interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, target)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		h, v := docStyle.GetFrameSize()
		listHeight := m.height - v - 5
		paneWidth := (m.width - h - 2) /int(NumPanes) // -2 for borders between panes

		m.histList.SetSize(paneWidth, listHeight)
		m.deviceList.SetSize(paneWidth, listHeight)
		m.logView.Width = paneWidth
		m.logView.Height = listHeight

		// Set help width
		m.help.Width = m.width - h

		m.ready = true
		m.logView.GotoBottom() // Scroll log to bottom on resize

	case tea.KeyMsg:
		// Handle keys even if lists have focus for global actions
		switch {
		case key.Matches(msg, m.keys.Quit):
			m.logf("Quitting...")
			if m.wsCtxCancel != nil {
				m.wsCtxCancel() // Signal background tasks to stop
			}
			if m.wsConn != nil {
				// Attempt clean close
				m.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				m.wsConn.Close()
			}
			return m, tea.Quit

		case key.Matches(msg, m.keys.ToggleSync):
			m.syncEnabled = !m.syncEnabled
			m.logf("Clipboard sync %s", map[bool]string{true: "enabled", false: "disabled"}[m.syncEnabled])
			// Maybe send status to server? Optional.
			return m, nil

		case key.Matches(msg, m.keys.FocusNext):
			m.focus = (m.focus + 1) % NumPanes
			m.updateFocus()
			return m, nil
		case key.Matches(msg, m.keys.FocusPrev):
			m.focus = (m.focus - 1 + NumPanes) % NumPanes
			m.updateFocus()
			return m, nil

		case key.Matches(msg, m.keys.AcceptFile):
			if m.incomingFileOffer != nil {
				m.logf("Accepting file offer for '%s' from %s", m.incomingFileOffer.Filename, m.devicesMap[m.offeringClientID])
				ack := BaseMessage{
					Type: "file_ack",
					Data: FileAckData{Filename: m.incomingFileOffer.Filename, Allow: true, SourceID: m.offeringClientID},
				}
				cmds = append(cmds, sendWebsocketMessageCmd(m.wsConn, ack))
				// TODO: Prepare to receive file chunks
				m.incomingFileOffer = nil // Clear offer state
			}
			return m, tea.Batch(cmds...)

		case key.Matches(msg, m.keys.RejectFile):
			if m.incomingFileOffer != nil {
				m.logf("Rejecting file offer for '%s'", m.incomingFileOffer.Filename)
				ack := BaseMessage{
					Type: "file_ack",
					Data: FileAckData{Filename: m.incomingFileOffer.Filename, Allow: false, SourceID: m.offeringClientID},
				}
				cmds = append(cmds, sendWebsocketMessageCmd(m.wsConn, ack))
				m.incomingFileOffer = nil // Clear offer state
			}
			return m, tea.Batch(cmds...)

		case key.Matches(msg, m.keys.InitiateXfer):
			if m.focus == DevicesPane && m.deviceList.SelectedItem() != nil {
				selectedDevice := m.deviceList.SelectedItem().(deviceItem)
				if selectedDevice.ID == "" { // Don't xfer to self or unknown
					m.logf("Cannot initiate transfer with selected device.")
					return m, nil
				}
				m.logf("Initiating file transfer with %s (Not Implemented)", selectedDevice.Hostname)
				// TODO: Implement file selection (needs external library or input field)
				// 1. Prompt for file path
				// 2. Get file size
				// 3. Send file_offer message
			}
			return m, nil
		}

		// If not a global key, pass to the focused component
		switch m.focus {
		case HistoryPane:
			m.histList, cmd = m.histList.Update(msg)
			cmds = append(cmds, cmd)
		case DevicesPane:
			m.deviceList, cmd = m.deviceList.Update(msg)
			cmds = append(cmds, cmd)
		case LogPane:
			// Allow scrolling in log view
			m.logView, cmd = m.logView.Update(msg)
			cmds = append(cmds, cmd)
		}

	case spinner.TickMsg:
		if m.connectedState == Connecting {
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	// --- Connection and App Logic Messages ---
	case ConnectionStatusMsg:
		m.connectedState = msg.Status
		m.lastError = msg.Err // Store error even on success (becomes nil)

		if msg.Status == Connected && msg.Conn != nil {
			m.wsConn = msg.Conn
			m.wsCtxCancel = msg.Cancel
			m.logf("Connected to server.")
			// Start the listener and clipboard checker *after* connection established
			cmds = append(cmds, listenWebSocketCmd(context.Background(), m.wsConn, m.programRef)) // Pass program ref!
			cmds = append(cmds, checkLocalClipboardCmd(m.lastSentClip)) // Initial check
			// Request initial device list from server
			cmds = append(cmds, sendWebsocketMessageCmd(m.wsConn, BaseMessage{Type: "request_devices"}))

		} else { // Disconnected or Error during connection
			if m.wsCtxCancel != nil {
				m.wsCtxCancel() // Ensure context is cancelled
				m.wsCtxCancel = nil
			}
			m.wsConn = nil
			if msg.Err != nil {
				m.logf("Connection Error: %v", msg.Err)
				// Schedule reconnect attempt?
				// cmds = append(cmds, tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
				// 	 return connectCmd(m.serverURL, m.apiKey, m.hostname)
				// }))
			} else {
				m.logf("Disconnected.")
			}
		}

	case ReceivedServerMsg: // Process messages received via WebSocket listener
		serverMsg := msg.Msg
		m.logf("Server -> Type: %s", serverMsg.Type) // Log received type

		switch serverMsg.Type {
		case "clipboard_update":
			var data ClipboardUpdateData
			if err := RemarshalData(serverMsg.Data, &data); err == nil {
				m.lastRcvdClip = data.Content
				// Add to history list
				m.histList.InsertItem(0, historyItem(data.Content))
				if len(m.histList.Items()) > maxHistorySize {
					m.histList.RemoveItem(len(m.histList.Items()) - 1)
				}
				// Write to local clipboard if sync enabled and not an echo
				if m.syncEnabled && data.Content != m.lastSentClip {
					cmds = append(cmds, writeToClipboardCmd(data.Content))
				}
			} else {
				m.logf("Error decoding clipboard_update: %v", err)
			}

		case "clipboard_history":
			var data ClipboardHistoryData
			if err := RemarshalData(serverMsg.Data, &data); err == nil {
				newHist := make([]list.Item, len(data.History))
				for i, h := range data.History {
					newHist[i] = historyItem(h)
				}
				m.histList.SetItems(newHist)
				m.logf("Received clipboard history (%d items)", len(newHist))
			} else {
				m.logf("Error decoding clipboard_history: %v", err)
			}

		case "device_list":
			var data DeviceListData
			if err := RemarshalData(serverMsg.Data, &data); err == nil {
				devItems := make([]list.Item, 0, len(data.Devices))
				m.devicesMap = make(map[string]string) // Reset map
				for _, d := range data.Devices {
					// Don't list self (based on hostname potentially?)
					// Or server could filter based on SenderID if request initiated it
					// For now, list all received
					devItems = append(devItems, deviceItem(d))
					m.devicesMap[d.ID] = d.Hostname // Store for lookup
				}
				m.deviceList.SetItems(devItems)
				m.logf("Updated device list (%d devices)", len(devItems))
			} else {
				m.logf("Error decoding device_list: %v", err)
			}

		case "file_offer":
			var data FileOfferData
			if err := RemarshalData(serverMsg.Data, &data); err == nil {
				senderHostname := m.devicesMap[serverMsg.SenderID] // Lookup hostname
				if senderHostname == "" {
					senderHostname = serverMsg.SenderID // Fallback to ID
				}
				m.logf(">>> Incoming file offer: '%s' (%d bytes) from %s", data.Filename, data.Filesize, senderHostname)
				m.logf(">>> Press 'a' to accept, 'r' to reject.")
				m.incomingFileOffer = &data
				m.offeringClientID = serverMsg.SenderID // Store sender ID
			} else {
				m.logf("Error decoding file_offer: %v", err)
			}

		case "file_ack":
			var data FileAckData
			if err := RemarshalData(serverMsg.Data, &data); err == nil {
				receiverHostname := m.devicesMap[serverMsg.SenderID] // Receiver sent the ACK
				if receiverHostname == "" {
					receiverHostname = serverMsg.SenderID
				}
				if data.Allow {
					m.logf("'%s' accepted file '%s'. Starting transfer (Not Implemented)", receiverHostname, data.Filename)
					// TODO: Implement command to start sending file chunks
				} else {
					m.logf("'%s' rejected file '%s'.", receiverHostname, data.Filename)
				}
			} else {
				m.logf("Error decoding file_ack: %v", err)
			}

		default:
			m.logf("Received unhandled server message type: %s", serverMsg.Type)
		}

	case LocalClipboardCheckedMsg:
		if m.connectedState != Connected { // Don't process if not connected
			return m, nil
		}
		if msg.Err != nil {
			// m.logf("Clipboard read error: %v", msg.Err) // Reduce log noise
			return m, nil
		}
		// If sync enabled, content changed, and it's not an echo of what we just received
		if m.syncEnabled && msg.Changed && msg.Content != m.lastRcvdClip {
			m.logf("Local clipboard changed, sending update...")
			m.lastSentClip = msg.Content
			updateMsg := BaseMessage{
				Type: "clipboard_update",
				Data: ClipboardUpdateData{Content: msg.Content},
			}
			cmds = append(cmds, sendWebsocketMessageCmd(m.wsConn, updateMsg))
		}
		// Schedule the next check regardless of change
		cmds = append(cmds, tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
			// Pass the *current* lastSentClip value when scheduling the next check
			return checkLocalClipboardCmd(m.lastSentClip)()
		}))

	case ErrorMsg:
		m.lastError = msg.Err
		m.logf("Error: %v", msg.Err)

	case LogMsg:
		m.logf(string(msg))

	} // End main switch

	// Update spinner if needed (outside main switch)
	if m.connectedState == Connecting {
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	// Handle focus updates separate from keypress logic
	m.updateFocus()

	return m, tea.Batch(cmds...)
}

// updateFocus ensures the correct components are focused/blurred
func (m *Model) updateFocus() {
	m.histList.SetShowPagination(m.focus == HistoryPane)
	m.histList.SetShowFilter(m.focus == HistoryPane)
	m.deviceList.SetShowPagination(m.focus == DevicesPane)
	m.deviceList.SetShowFilter(m.focus == DevicesPane)
	m.logView.MouseWheelEnabled = (m.focus == LogPane)

}


func (m Model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	status := fmt.Sprintf(" Status: %s", m.connectedState)
	if m.connectedState == Connecting {
		status += " " + m.spinner.View()
	}
	if m.lastError != nil {
		status = fmt.Sprintf(" Status: %s | %s", m.connectedState, errorStyle.Render(m.lastError.Error()))
	}
	statusView := statusStyle.Width(m.width).Render(status)

	// Sync Status
	syncText := "OFF"
	if m.syncEnabled {
		syncText = "ON"
	}
	syncView := syncStatusStyle.Render(fmt.Sprintf("Sync: %s", syncText))

	// Combine Status and Sync
	statusBar := lipgloss.JoinHorizontal(lipgloss.Top,
		statusView, // Let status take available width
		lipgloss.NewStyle().PaddingLeft(1).Render(syncView),
	)

	// Panes
	histPane := getPaneStyle(m.focus == HistoryPane).Render(m.histList.View())
	devPane := getPaneStyle(m.focus == DevicesPane).Render(m.deviceList.View())
	logPane := getPaneStyle(m.focus == LogPane).Render(m.logView.View())

	// Combine Panes Horizontally
	panes := lipgloss.JoinHorizontal(lipgloss.Top, histPane, devPane, logPane)

	// Help View
	helpView := helpStyle.Render(m.help.View(m.keys))
	if m.incomingFileOffer != nil {
	offerHelp := lipgloss.JoinHorizontal(lipgloss.Left,
			// Correct way: Create style -> Set Color -> Render
			lipgloss.NewStyle().Foreground(special).Render(fmt.Sprintf("Offer: '%s' ", m.incomingFileOffer.Filename)),
			m.keys.AcceptFile.Help().Key+" accept", " | ",
			m.keys.RejectFile.Help().Key+" reject",
		)
		helpView = lipgloss.JoinVertical(lipgloss.Left, offerHelp, helpView)
	}


	// Final Layout
	return docStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
		statusBar,
		panes,
		helpView,
	))
}

// Helper to add log messages with scrolling
func (m *Model) logf(format string, args ...interface{}) {
	now := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", now, fmt.Sprintf(format, args...))
	m.logMessages = append(m.logMessages, logEntry)

	// Optional: Limit log buffer size
	const maxLogLines = 200
	if len(m.logMessages) > maxLogLines {
		m.logMessages = m.logMessages[len(m.logMessages)-maxLogLines:]
	}

	m.logView.SetContent(strings.Join(m.logMessages, "\n"))
	m.logView.GotoBottom() // Scroll to bottom
	log.Println(logEntry)  // Also log to file
}
