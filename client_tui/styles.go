package main

import "github.com/charmbracelet/lipgloss"

var (
	// General
	subtle    = lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"}
	highlight = lipgloss.AdaptiveColor{Light: "#874BFD", Dark: "#7D56F4"}
	special   = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}

	// Layout
	docStyle = lipgloss.NewStyle().Margin(1, 2)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#343433", Dark: "#C1C6B2"}).
			Background(highlight)

	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5E5E"))

	syncStatusStyle = lipgloss.NewStyle().Foreground(special)

	paneStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)

	focusedPaneStyle = paneStyle.Copy().BorderForeground(highlight)

	helpStyle = lipgloss.NewStyle().Foreground(subtle)

	// List styles (can be customized further)
	listTitleStyle = lipgloss.NewStyle().
			Background(highlight).
			Foreground(lipgloss.Color("#FFF")).
			Padding(0, 1)

	listHelpStyle = helpStyle.Copy()
)

// Function to get pane style based on focus
func getPaneStyle(focused bool) lipgloss.Style {
	if focused {
		return focusedPaneStyle
	}
	return paneStyle
}
