package tui

import "github.com/charmbracelet/lipgloss"

var (
	accent  = lipgloss.Color("#f06292") // stonefruit pink
	muted   = lipgloss.Color("244")
	success = lipgloss.Color("#8bc34a")
	danger  = lipgloss.Color("#ef5350")

	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	bannerStyle  = lipgloss.NewStyle().Foreground(accent)
	mutedStyle   = lipgloss.NewStyle().Foreground(muted)
	successStyle = lipgloss.NewStyle().Foreground(success).Bold(true)
	dangerStyle  = lipgloss.NewStyle().Foreground(danger).Bold(true)
	labelStyle   = lipgloss.NewStyle().Bold(true)
	hintStyle    = lipgloss.NewStyle().Foreground(muted).Italic(true)

	checkMark = successStyle.Render("✓")
	crossMark = dangerStyle.Render("✗")
	bullet    = mutedStyle.Render("•")

	frameStyle = lipgloss.NewStyle().
			Padding(1, 2).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(accent)
)

const banner = `     _                      __            _ _
    | |                    / _|          (_) |
 ___| |_ ___  _ __   ___  | |_ _ __ _   _ _| |_
/ __| __/ _ \| '_ \ / _ \ |  _| '__| | | | | __|
\__ \ || (_) | | | |  __/ | | | |  | |_| | | |_
|___/\__\___/|_| |_|\___| |_| |_|   \__,_|_|\__|`
