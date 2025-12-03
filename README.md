# Agent Deck 🎛️

A universal terminal session manager for AI coding agents and any terminal work.

![Agent Deck](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/License-MIT-green.svg)

```
┌─────────────────────────────────────────────────────────────────────────┐
│  🎛️  Agent Deck                              8 sessions   [/] Search    │
├────────────────────────┬────────────────────────────────────────────────┤
│  📁 Projects           │  Preview: devops/claude-1                      │
│  ▼ projects (4)     ● │  $ claude                                      │
│    ▶ devops/claude  ● │  I'll help you with the deployment...          │
│      devops/shell   ○ │                                                │
│      frontend       ◌ │  ┌─────────────────────────────────────────┐   │
│  ▼ personal (2)        │  │ Proceed with changes? (Y/n)             │   │
│      blog           ◌ │  └─────────────────────────────────────────┘   │
├────────────────────────┴────────────────────────────────────────────────┤
│ [↑↓] Navigate [Enter] Attach [/] Search [n] New [Tab] Fold [d] Del [q]  │
└─────────────────────────────────────────────────────────────────────────┘
```

## Why Agent Deck?

- **Universal** - Works with any terminal program: Claude Code, Gemini CLI, Aider, Codex, or plain shell
- **Fast** - Instant session creation (no forced program startup)
- **Organized** - Project-first hierarchy with collapsible folders
- **Searchable** - Find any session instantly with `/` search
- **Rock solid** - Built on tmux, battle-tested for 20+ years
- **Minimal** - ~5MB binary, low memory footprint

## Installation

### Prerequisites

- macOS or Linux
- [tmux](https://github.com/tmux/tmux) (`brew install tmux` or `apt install tmux`)
- [Go 1.21+](https://go.dev/dl/) (for building from source)

### From Source

```bash
git clone https://github.com/asheshgoplani/agent-deck.git
cd agent-deck
make install
```

### Build Only

```bash
make build
./build/agent-deck
```

## Usage

```bash
agent-deck
```

## Key Bindings

### In Agent Deck
| Key | Action |
|-----|--------|
| `↑` / `k` | Move up |
| `↓` / `j` | Move down |
| `Enter` | Attach to session |
| `/` | Search sessions |
| `n` | New session |
| `Tab` | Collapse/expand folder |
| `d` | Delete session |
| `i` | Import existing tmux sessions |
| `r` | Refresh |
| `q` | Quit |

### While Attached to a Session
| Key | Action |
|-----|--------|
| `Ctrl+Q` | **Detach** and return to Agent Deck |

> **Note**: Press `Ctrl+Q` to instantly detach from the session and return to the main menu.

## Supported Tools

| Icon | Tool |
|------|------|
| 🤖 | Claude Code |
| ✨ | Gemini CLI |
| 🔧 | Aider |
| 💻 | Codex |
| 🐚 | Shell / Other |

## Status Indicators

- `●` **Running** (green) - Session has active process
- `○` **Waiting** (yellow) - Detected prompt waiting for input
- `◌` **Idle** (gray) - Session exists but at shell prompt
- `✕` **Error** (red) - Session has an error

## Hook Integration (Instant Status Updates)

Agent Deck can receive instant status updates from AI tools via hooks, eliminating polling delay.

### Quick Setup

```bash
# Install hooks for Claude Code and Gemini CLI
agent-deck hooks install
```

### Manual Setup

**Claude Code** (`~/.claude/settings.json`):
```json
{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "~/.agent-deck/hooks/claude-code.sh"}]}],
    "Notification": [{"matcher": "idle_prompt|permission_prompt", "hooks": [{"type": "command", "command": "~/.agent-deck/hooks/claude-code.sh"}]}]
  }
}
```

**Gemini CLI** (`~/.gemini/settings.json`):
```json
{
  "hooks": {
    "AfterAgent": [{"type": "command", "command": "~/.agent-deck/hooks/gemini-cli.sh"}],
    "Notification": [{"type": "command", "command": "~/.agent-deck/hooks/gemini-cli.sh"}]
  }
}
```

### Connection Status

The header shows which tools are connected:
- **Claude ✓** - Claude Code hooks are active
- **Gemini ✓** - Gemini CLI hooks are active
- **Tool ○** - Not connected (will fall back to polling)

### How It Works

1. Agent Deck starts a Unix socket server at `/tmp/agent-deck.sock`
2. When an AI tool finishes (Stop/AfterAgent event), it sends a JSON message
3. Agent Deck instantly updates the session status to YELLOW (waiting)
4. No more flickering from polling delays!

## Configuration

State is stored in `~/.agent-deck/`:
- `sessions.json` - Saved sessions
- `hooks/` - Hook scripts (after running `agent-deck hooks install`)

## Development

```bash
# Run tests
make test

# Run with auto-reload
make dev

# Format code
make fmt

# Lint code
make lint

# Build for all platforms
make release
```

## Project Structure

```
agent-deck/
├── cmd/agent-deck/     # Entry point
├── internal/
│   ├── ui/             # TUI components
│   ├── session/        # Session management
│   └── tmux/           # tmux integration
├── Makefile
└── README.md
```

## Comparison with claude-squad

| Feature | Agent Deck | claude-squad |
|---------|------------|--------------|
| Universal sessions | ✅ Any tool | ❌ Claude only |
| Fast session creation | ✅ Instant | ❌ Slow startup |
| Project hierarchy | ✅ Folders | ❌ Flat list |
| Session search | ✅ Built-in | ❌ None |
| Import existing | ✅ Yes | ❌ No |
| Memory usage | ~5MB | Higher |

## License

MIT License - see [LICENSE](LICENSE) for details.

## Credits

Built with:
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) - TUI framework
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) - Styling
- [tmux](https://github.com/tmux/tmux) - Terminal multiplexer
