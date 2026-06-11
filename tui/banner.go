// Package tui contains the terminal rendering layer of argus. It must stay
// free of privileged logic: rendering and presentation only.
package tui

import "github.com/charmbracelet/lipgloss"

// Banner is the Argus wordmark rendered as block ASCII art.
const Banner = `
 █████╗ ██████╗  ██████╗ ██╗   ██╗███████╗
██╔══██╗██╔══██╗██╔════╝ ██║   ██║██╔════╝
███████║██████╔╝██║  ███╗██║   ██║███████╗
██╔══██║██╔══██╗██║   ██║██║   ██║╚════██║
██║  ██║██║  ██║╚██████╔╝╚██████╔╝███████║
╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝  ╚═════╝ ╚══════╝`

// Tagline is the brand line shown beneath the wordmark.
const Tagline = "the all-seeing server console"

// Version is the argus release shown in the compact brand fallback.
const Version = "v0.1.0"

// minSplashWidth is the narrowest terminal (in columns) that fits the banner.
const minSplashWidth = 44

var (
	eyeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	markStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("44"))
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

// Splash renders the full Argus splash screen: eyes, wordmark and tagline.
func Splash() string {
	eyes := eyeStyle.Render("◉ ◦ ◉ ◦ ◉ ◦ ◉ ◦ ◉ ◦ ◉ ◦ ◉ ◦ ◉ ◦ ◉")
	art := markStyle.Render(Banner)
	tag := dimStyle.Render("        " + Tagline)
	return lipgloss.JoinVertical(lipgloss.Center, eyes, art, tag)
}

// SplashForWidth returns the full splash if the terminal is wide enough,
// otherwise the compact brand mark.
func SplashForWidth(width int) string {
	if width > 0 && width < minSplashWidth {
		return eyeStyle.Render("◉") + markStyle.Render(" argus") + dimStyle.Render("  ·  "+Version)
	}
	return Splash()
}
