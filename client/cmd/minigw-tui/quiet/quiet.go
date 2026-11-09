// Package quiet stops Bubble Tea v1 from querying the terminal at start. Bubble Tea's own init calls
// lipgloss.HasDarkBackground, which writes OSC 11 and CSI 6n and waits up to 5 s per byte for a reply that some
// terminals (serial, some SSH clients) never send, also eating the keys typed meanwhile. minigw-tui uses no
// colors, so the background is set here, before that init: Go initializes packages in import-path order once
// their imports are, and "dev.trojar/…" sorts before "github.com/charmbracelet/bubbletea" (Go 1.21+).
// The pty test in minigw-tui checks it.
package quiet

import "github.com/charmbracelet/lipgloss"

func init() { lipgloss.SetHasDarkBackground(true) }
