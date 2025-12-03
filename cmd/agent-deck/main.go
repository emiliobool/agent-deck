package main

import (
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/asheshgoplani/agent-deck/internal/ui"
)

const Version = "0.1.0"

func main() {
	// Handle subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("Agent Deck v%s\n", Version)
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	}

	// Check if tmux is available
	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Println("Error: tmux not found in PATH")
		fmt.Println("\nAgent Deck requires tmux. Install with:")
		fmt.Println("  brew install tmux")
		os.Exit(1)
	}

	p := tea.NewProgram(
		ui.NewHome(),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf("Agent Deck v%s\n", Version)
	fmt.Println("Terminal session manager for AI coding agents")
	fmt.Println()
	fmt.Println("Usage: agent-deck [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  (none)     Start the TUI")
	fmt.Println("  version    Show version")
	fmt.Println("  help       Show this help")
	fmt.Println()
	fmt.Println("Keyboard shortcuts (in TUI):")
	fmt.Println("  n          New session")
	fmt.Println("  g          New group")
	fmt.Println("  Enter      Attach to session")
	fmt.Println("  d          Delete session/group")
	fmt.Println("  m          Move session to group")
	fmt.Println("  R          Rename session/group")
	fmt.Println("  /          Search")
	fmt.Println("  Ctrl+Q     Detach from session")
	fmt.Println("  q          Quit")
}
