# Finn Desktop Daemon

A cross-platform desktop daemon that bridges Claude Code CLI with mobile and web clients for AI-powered coding.

## Overview

The Finn Desktop Daemon runs on your development machine and provides:

- **Claude Code Integration**: Executes Claude Code CLI tasks triggered from mobile/web
- **Git Operations**: Tracks changes, generates diffs, and manages commits
- **Session Watching**: Monitors external Claude Code sessions in approved folders
- **Live Preview**: Tunnels local dev servers for mobile preview
- **Folder Management**: Maintains a whitelist of approved project folders

## Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Mobile/Web     │────▶│   Relay Server   │◀────│ Desktop Daemon  │
│    Client       │◀────│   (WebSocket)    │────▶│ (This Project)  │
└─────────────────┘     └──────────────────┘     └─────────────────┘
                                                         │
                                                         ▼
                                                 ┌─────────────────┐
                                                 │  Claude Code    │
                                                 │      CLI        │
                                                 └─────────────────┘
```

## Installation

### Prerequisites

- Go 1.21 or later
- [Claude Code CLI](https://claude.ai/code) installed and authenticated
- Anthropic API key set in environment (`ANTHROPIC_API_KEY`)

### Build from Source

```bash
# Clone the repository
git clone https://github.com/getfinn/finn
cd finn

# Install dependencies
go mod download

# Build
go build -o finn cmd/finn/main.go

# Or build for specific platform
GOOS=darwin GOARCH=arm64 go build -o finn-darwin-arm64 cmd/finn/main.go
GOOS=windows GOARCH=amd64 go build -o finn-windows-amd64.exe cmd/finn/main.go
GOOS=linux GOARCH=amd64 go build -o finn-linux-amd64 cmd/finn/main.go
```

## Usage

### GUI Mode (Default)

```bash
./finn
```

This starts the daemon with a system tray icon for managing folders and viewing status.

### Headless Mode

```bash
./finn --headless
```

Runs without GUI - useful for servers or remote development machines.

### Development Mode

```bash
./finn --dev
```

Uses development configuration (connects to `localhost:8080` instead of production).

## System Tray Menu

- **Status**: Shows connection status
- **Projects**: List of approved project folders
  - **+ Add Project Folder...**: Opens native file picker
- **Open Web Dashboard**: Opens the web dashboard
- **Quit**: Stops the daemon

## Configuration

Configuration is stored in OS-specific locations:

- **macOS**: `~/Library/Application Support/finn/config.json`
- **Windows**: `%APPDATA%/finn/config.json`
- **Linux**: `~/.config/finn/config.json`

### Example Config

```json
{
  "user_id": "user_abc123",
  "device_id": "desktop-hostname-1234567890",
  "auth_tokens": {
    "wss://api.tryfinn.ai/ws": "jwt-token-here"
  },
  "approved_folders": [
    {
      "id": "uuid-v4",
      "name": "my-project",
      "path": "/Users/you/Projects/my-project"
    }
  ],
  "selected_folder_id": "uuid-v4",
  "subscription": {
    "tier": "standard",
    "max_folders": 5,
    "active": true
  },
  "execution_mode": {
    "interactive_mode": true,
    "diff_approval_mode": "show-all"
  }
}
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `FINN_RELAY_URL` | WebSocket URL for relay server | `wss://api.tryfinn.ai/ws` |
| `FINN_DASHBOARD_URL` | Dashboard URL for OAuth | `https://tryfinn.ai` |
| `ANTHROPIC_API_KEY` | API key for Claude Code | (required) |

## Security Model

### Folder Approval

The daemon only operates on explicitly approved folders. Users must approve folders via:
- System tray menu (GUI mode)
- Web dashboard
- Mobile app

### Claude Code Permissions

This daemon uses `--dangerously-skip-permissions` when running Claude Code. This is safe because:

1. **Folder Whitelist**: Code only runs in user-approved directories
2. **Git-based Review**: All changes are captured as diffs for review before committing
3. **User Approval Flow**: Changes require explicit approval before persisting
4. **Multi-turn Conversation**: Users can revise or reject changes at any point

See `internal/claude/doc.go` for detailed security documentation.

## Project Structure

```
cmd/
  finn/
    main.go              # Entry point

internal/
  agent/
    agent.go             # Core agent lifecycle
    agent_auth.go        # OAuth authentication
    agent_execution.go   # Claude task execution
    agent_folders.go     # Folder management
    agent_git.go         # Git operations
    agent_handlers.go    # WebSocket message routing
    agent_preview.go     # Live preview tunnels
    agent_sessions.go    # Session watching

  auth/                  # OAuth flow handling
  claude/               # Claude Code CLI integration
  config/               # Configuration management
  devserver/            # Dev server detection and management
  git/                  # Git operations
  tunnel/               # Cloudflare tunnel client
  ui/                   # System tray UI
  watcher/              # Claude session file watcher
  websocket/            # WebSocket client
```

## Development

### Running Locally

```bash
# Start relay server first
cd ../relay-server
go run cmd/relay/main.go

# Then start daemon with dev settings
cd ../desktop-daemon
go run cmd/finn/main.go --dev

# Or headless mode
go run cmd/finn/main.go --headless --dev
```

### Hot Reload

For rapid development, use Air:

```bash
go install github.com/cosmtrek/air@latest
air
```

### Testing

```bash
go test ./...
```

## WebSocket Message Types

The daemon communicates via WebSocket with these message types:

### Incoming (from Mobile/Web)

| Type | Description |
|------|-------------|
| `prompt` | Execute Claude Code task |
| `choice` | User's choice for decision point |
| `approval` | Approve/reject all diffs |
| `diff_approved` | Approve specific file diff |
| `reprompt` | Continue conversation with new prompt |
| `folder_add_request` | Add folder to whitelist |
| `folder_remove_request` | Remove folder from whitelist |
| `folder_select` | Select active folder |
| `browse_folders` | Browse filesystem |
| `preview_start` | Start live preview tunnel |
| `preview_stop` | Stop live preview |
| `get_commits` | Request commit history |
| `get_commit_detail` | Request specific commit details |
| `resume_session` | Resume external Claude session |
| `get_external_sessions` | List external sessions |
| `get_session_messages` | Get messages from session |

### Outgoing (to Mobile/Web)

| Type | Description |
|------|-------------|
| `thinking` | Claude's reasoning process |
| `tool_use` | Tool execution progress |
| `decision` | AskUserQuestion prompt |
| `diff` | File change diff for review |
| `complete` | Task completed |
| `error` | Error occurred |
| `preview_ready` | Preview URL available |
| `preview_status` | Preview status update |
| `folder_list` | Approved folders list |
| `folder_response` | Response to folder operation |
| `commits_list` | Commit history |
| `commit_detail` | Single commit details |
| `commit_success` | Commit created |
| `external_session_detected` | New Claude session found |
| `external_session_updated` | Session metadata changed |
| `session_messages` | Session message history |

## Cross-Platform Notes

### macOS

- System tray uses native Cocoa APIs via cgo
- Supports both Intel (amd64) and Apple Silicon (arm64)
- File dialogs use native NSOpenPanel

### Windows

- System tray uses native Windows APIs
- Uses `NUL` instead of `/dev/null` for git operations
- Build with `GOOS=windows GOARCH=amd64`

### Linux

- Requires desktop environment with system tray support (AppIndicator)
- Tested on GNOME, KDE, and XFCE
- May require `libappindicator3-1` package

## Troubleshooting

**System tray doesn't appear:**
- Ensure you have proper permissions for menu bar items
- On macOS, check System Settings > Privacy & Security
- On Linux, ensure AppIndicator support is installed

**Can't connect to relay:**
- Verify relay server is running
- Check JWT token in config
- Review logs for connection errors
- Ensure network connectivity to relay URL

**Claude Code not executing:**
- Verify `claude` CLI is installed and in PATH
- Ensure `ANTHROPIC_API_KEY` is set
- Check that folder is approved

**File picker doesn't open:**
- Requires GUI environment (won't work over SSH)
- Use `--headless` mode and add folders via web dashboard

## License

MIT License - See LICENSE file for details.
