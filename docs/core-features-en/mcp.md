# MCP Tool Integration Technical Design

harness9's MCP integration lets Agents seamlessly call any external tool server that follows the [Model Context Protocol](https://modelcontextprotocol.io) specification. MCP tools are injected into the unified tool registry in the `mcp__{server}__{tool}` format, fully transparent to the Engine — concurrent execution, timeout control, error propagation, and all other existing mechanisms apply automatically, with zero changes to the core loop.

---

## Architecture Overview

```
.mcp.json
    ↓ LoadConfig
Config (Servers map)
    ↓ Manager.Start (concurrent goroutines, 30s timeout/server, fail-soft)
    ↓ newTransport (stdio / http)
    ↓ Client.Connect (initialize → notifications/initialized → tools/list)
ServerStatus + ToolDetails
    ↓ sendNotify → mcpNotifyCh → TUI MCPBar / /mcp panel
    ↓ Manager.InjectTools (MCPToolAdapter per tool)
tools.Registry
    ↓ Engine.runLoop (LLM call carries the full tool list)
LLM → ToolCall (mcp__context7__resolve_library_id)
    ↓ Registry.Execute → MCPToolAdapter.Execute
    ↓ Client.CallTool (tools/call JSON-RPC)
MCP Server → tool result → Observation injected into context
```

---

## Configuration (`.mcp.json`)

harness9 loads the MCP configuration from `.mcp.json` in the project root on every startup. If the file does not exist, it is silently ignored and startup is unaffected.

### Format

```json
{
  "mcpServers": {
    "context7": {
      "command": "npx",
      "args": ["-y", "@upstash/context7-mcp"]
    },
    "remote-api": {
      "type": "http",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_TOKEN"
      }
    }
  }
}
```

### ServerConfig Fields

| Field | Type | Description |
|------|------|------|
| `type` | string | Transport type: `"stdio"` or `"http"`; auto-inferred from `command` / `url` when omitted |
| `command` | string | Executable for a stdio server (e.g. `"npx"`, `"python"`) |
| `args` | []string | Command-line arguments (e.g. `["-y", "@upstash/context7-mcp"]`) |
| `env` | []string | Additional environment variables injected into the subprocess (`"KEY=VALUE"` format) |
| `url` | string | HTTP server address (required when type=http) |
| `headers` | map | HTTP request headers (e.g. Bearer Token authentication) |

### Loading Logic

```go
// internal/mcp/config.go
func LoadConfig(path string) (Config, error) {
    data, err := os.ReadFile(path)
    if os.IsNotExist(err) {
        return Config{Servers: make(map[string]ServerConfig)}, nil  // silently return an empty config
    }
    // ...
}
```

When the file does not exist, an empty `Config` is returned without error — with no MCP configuration, harness9's behavior is exactly identical to before this feature existed.

---

## Transport Layer (`internal/mcp/transport.go`)

### Transport Interface

```go
type Transport interface {
    Start(ctx context.Context) error
    Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
    Notify(method string, params json.RawMessage) error
    Close() error
}
```

All transport implementations satisfy this interface, so the Client layer is fully transparent to the concrete transport in use.

### StdioTransport (primary implementation)

The stdio transport implements the JSON-RPC 2.0 protocol over a subprocess's stdin/stdout. Each message is one complete line of JSON (NDJSON), correlated with requests and responses via a numeric ID.

**Core data structure:**

```go
type StdioTransport struct {
    command string
    args    []string
    env     []string

    cmd  *exec.Cmd
    enc  *json.Encoder  // writes to stdin (protected by writeMu)
    scan *bufio.Scanner // reads from stdout

    writeMu sync.Mutex          // protects enc (multiple goroutines writing to stdin concurrently)
    nextID  atomic.Int64        // monotonically increasing request ID
    mu      sync.Mutex          // protects the pending map
    pending map[int64]chan rpcResponse // ID → waiting channel
    done    chan struct{}        // readLoop exit signal
}
```

**Concurrency-safety design:**

- `writeMu`: protects writes to stdin — when multiple tool calls execute concurrently within the same Turn, each `Send` call needs serialized writes
- `mu`: protects the `pending` map — `Send` registers a channel and `readLoop` routes responses, both operating concurrently
- `done` channel: closed when the `readLoop` goroutine exits; `Send`'s three-way select detects transport-layer shutdown through it

**readLoop goroutine:**

```go
func (t *StdioTransport) readLoop() {
    defer close(t.done)
    for t.scan.Scan() {
        line := t.scan.Bytes()
        var resp rpcResponse
        if err := json.Unmarshal(line, &resp); err != nil {
            continue // ignore non-JSON lines (e.g. startup logs from the MCP Server)
        }
        if resp.ID == nil {
            continue // a notification pushed proactively by the server; ignored in the current version
        }
        t.mu.Lock()
        ch, ok := t.pending[*resp.ID]
        if ok {
            delete(t.pending, *resp.ID)
        }
        t.mu.Unlock()
        if ok {
            ch <- resp
        }
    }
    // transport closed: notify all pending requests of failure to avoid callers blocking forever
    t.mu.Lock()
    defer t.mu.Unlock()
    for id, ch := range t.pending {
        ch <- rpcResponse{Error: &rpcError{Message: "transport closed"}}
        delete(t.pending, id)
    }
}
```

**Send's three-way select:**

```go
select {
case resp := <-ch:        // normal response
    if resp.Error != nil { return nil, fmt.Errorf("rpc error %d: %s", ...) }
    return resp.Result, nil
case <-ctx.Done():        // caller timeout/cancellation
    t.mu.Lock(); delete(t.pending, id); t.mu.Unlock()
    return nil, ctx.Err()
case <-t.done:            // transport unexpectedly closed
    return nil, fmt.Errorf("transport closed")
}
```

The three-way select guarantees a clean exit in all three scenarios — tool call timeout (configured via `WithToolTimeout`, default 60s), context cancellation (user Ctrl+C), and MCP Server process crash — without leaking goroutines or channels.

**Scanner buffer:**

`scan.Buffer(make([]byte, 1024*1024), 1024*1024)` sets a 1MB read buffer, accommodating scenarios where `tools/list` returns a large number of tool definitions (e.g. when some MCP Servers expose hundreds of tools, a single JSON response can exceed the default 64KB limit).

### HTTPTransport (stateless implementation)

The HTTP transport wraps each `Send` call as an independent `POST` request, with no persistent connection state. It applies to MCP Servers that expose a Streamable HTTP endpoint.

```go
// Start is a no-op: HTTP is stateless, no need to pre-establish a connection
func (t *HTTPTransport) Start(_ context.Context) error { return nil }

// Send: POST the JSON-RPC request body, parse the response
func (t *HTTPTransport) Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
    id := t.nextID.Add(1)
    req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
    // ... POST to t.url with t.headers ...
}
```

---

## Client (`internal/mcp/client.go`)

The Client encapsulates the MCP protocol handshake and tool-call logic.

### Handshake flow (Connect)

The MCP spec requires the client to complete a three-step handshake before it can start using tools:

```
1. Client → Server: {"jsonrpc":"2.0","id":1,"method":"initialize",
                      "params":{"protocolVersion":"2024-11-05",
                                "clientInfo":{"name":"harness9","version":"1.0.0"},
                                "capabilities":{}}}
   Server → Client: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},...}}

2. Client → Server: {"jsonrpc":"2.0","method":"notifications/initialized"}
   (a notification, no ID, the Server does not return a response)

3. Client → Server: {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
   Server → Client: {"jsonrpc":"2.0","id":2,"result":{"tools":[
     {"name":"resolve-library-id","description":"...","inputSchema":{...}},
     ...
   ]}}
```

Once the handshake completes, `client.Tools` is populated with the tool list, which the Manager uses to build `ToolDetails` and inject into the Registry.

### Tool call (CallTool)

```go
func (c *Client) CallTool(ctx context.Context, toolName string, args json.RawMessage) (string, error) {
    // build the tools/call request
    raw, _ := json.Marshal(struct {
        Name      string          `json:"name"`
        Arguments json.RawMessage `json:"arguments"`
    }{Name: toolName, Arguments: args})

    result, err := c.transport.Send(ctx, "tools/call", raw)
    // ...

    // extract text-type content blocks
    var parts []string
    for _, block := range callResult.Content {
        if block.Type == "text" && block.Text != "" {
            parts = append(parts, block.Text)
        }
    }
    output := strings.Join(parts, "\n")

    if callResult.IsError {
        return output, fmt.Errorf("tool %s returned error: %s", toolName, output)
    }
    return output, nil
}
```

The `content` returned by MCP's `tools/call` is an array, where each element can be of type `text`, `image`, or `resource`. harness9 currently extracts and concatenates all `text` blocks into a string, which covers the vast majority of real-world MCP Server response formats.

---

## Manager (`internal/mcp/manager.go`)

The Manager is the single coordinator for the lifecycle of multiple servers; it holds all active Client instances and notifies the TUI of status changes.

### Concurrent startup (Start)

```go
func (m *Manager) Start(ctx context.Context) error {
    var wg sync.WaitGroup
    for name, cfg := range m.config.Servers {
        wg.Add(1)
        go func(name string, cfg ServerConfig) {  // an independent goroutine per server
            defer wg.Done()

            // emit a pending status notification first, so the TUI immediately shows "connecting"
            m.mu.Lock()
            m.status[name] = ServerStatus{Name: name, Status: StatusPending}
            m.mu.Unlock()
            m.sendNotify()

            connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
            defer cancel()

            client, err := m.connect(connCtx, name, cfg)
            if err != nil {
                // fail-soft: record the failure status without blocking other servers' connections
                m.status[name] = ServerStatus{..., Status: StatusFailed, ErrMsg: err.Error()}
                m.sendNotify()
                return
            }

            // success: build ToolDetails and notify the TUI
            toolDetails := buildToolDetails(name, client.Tools)
            m.status[name] = ServerStatus{..., Status: StatusConnected, ToolDetails: toolDetails}
            m.sendNotify()
        }(name, cfg)
    }
    wg.Wait()
    return nil
}
```

**Design decision: fail-soft**

A single server connection failure (e.g. npx not installed, network unreachable) does not return an error, does not block the initialization of other servers, and does not prevent harness9 from starting. The failure status remains visible in the TUI MCPBar so the user is aware of it without being blocked.

**Design decision: asynchronous invocation**

In `main.go`, `Manager.Start` runs in its own goroutine:

```go
go func() {
    if err := mcpMgr.Start(ctx); err != nil { ... }
    injected := mcpMgr.InjectTools(registry)
}()
```

This avoids blocking TUI rendering on the cold start of `npx`-style servers (downloading dependencies can take tens of seconds). Users can start using built-in tools as soon as the TUI opens, while MCP tools are wired in silently in the background, with the MCPBar status updating in real time.

### InjectTools

```go
func (m *Manager) InjectTools(registry tools.Registry) int {
    m.mu.RLock()
    defer m.mu.RUnlock()

    count := 0
    for serverName, client := range m.clients {
        for _, toolInfo := range client.Tools {
            // key: capture into local variables to avoid the classic Go closure-over-loop-variable bug
            capturedServer := serverName
            capturedTool   := toolInfo
            capturedClient := client

            adapter := tools.NewMCPToolAdapter(
                capturedServer,
                capturedTool.Name,
                capturedTool.Description,
                capturedTool.InputSchema,
                func(ctx context.Context, args json.RawMessage) (string, error) {
                    return capturedClient.CallTool(ctx, capturedTool.Name, args)
                },
            )
            if err := registry.Register(adapter); err != nil {
                // skip on name conflicts without interrupting injection (built-in tools with the same name take priority)
                continue
            }
            count++
        }
    }
    return count
}
```

### sendNotify (lock-free notification)

```go
func (m *Manager) sendNotify() {
    if m.notify == nil { return }
    statuses := m.Statuses() // Statuses acquires an RLock internally, called outside of mu
    m.notify(statuses)       // does not hold mu, avoiding potential deadlocks from within the callback
}
```

`sendNotify` is designed to be called after the lock has been released. If `notify` were called while holding the lock, and the `notify` callback (e.g. sending to a channel) happened to block and trigger another operation requiring `m.mu`, a deadlock would result.

---

## MCPToolAdapter (`internal/tools/mcp_adapter.go`)

MCPToolAdapter is the sole entry point through which MCP tools enter harness9's tool system, implementing the `tools.BaseTool` interface:

```go
type MCPCallerFn func(ctx context.Context, args json.RawMessage) (string, error)

type MCPToolAdapter struct {
    adapterName string
    def         schema.ToolDefinition
    caller      MCPCallerFn
}

func (a *MCPToolAdapter) Name() string                        { return a.adapterName }
func (a *MCPToolAdapter) Definition() schema.ToolDefinition   { return a.def }
func (a *MCPToolAdapter) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    return a.caller(ctx, args)
}
```

### Naming convention: `mcp__{server}__{tool}`

Tool names use a double-underscore prefix format, consistent with the Claude Agent SDK and OpenHarness:

```
mcp__context7__resolve_library_id
mcp__context7__get_library_docs
mcp__github__list_issues
```

**Why double underscores**: a single underscore (OpenCode's approach: `context7_resolve_library_id`) risks ambiguity with ordinary tool names; double underscores make it visually and syntactically clear which tools are MCP tools versus built-in tools.

**sanitizeMCPName**: non-alphanumeric characters in the tool name or server name (such as `-`, `.`, spaces) are uniformly replaced with `_`:

```go
func sanitizeMCPName(name string) string {
    var sb strings.Builder
    for _, r := range name {
        if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
            sb.WriteRune(r)
        } else {
            sb.WriteRune('_')
        }
    }
    return sb.String()
}
```

Example: `resolve-library-id` → `resolve_library_id`, `my server` → `my_server`.

### Transparency to the Engine

Once MCPToolAdapter satisfies the `tools.BaseTool` interface, the Engine's main loop treats it exactly the same as built-in tools like `BashTool` and `ReadFileTool`:

- **Concurrent execution**: multiple MCP tools within the same Turn execute concurrently, each call occupying its own tool goroutine
- **Timeout control**: `WithToolTimeout` (default 60s) is passed via ctx through to `CallTool` → `Transport.Send`'s three-way select, and cancels automatically on timeout
- **Error propagation**: on tool execution failure, `Registry.Execute` wraps the result as `ToolResult{IsError: true}`, and the error message is passed back to the LLM as-is to trigger self-healing retries

---

## TUI Integration

### MCPBar (status bar)

MCPBar is displayed below the SandboxBar, rendered only when there are configured MCP Servers:

```
[MCP] context7 ● 2 tools │ remote-api ✗ failed
```

Status color coding:

| Status | Color | Meaning |
|------|------|------|
| Green `●` | Color("10") | Connected, tools available |
| Yellow `○` | Color("11") | Connecting (pending) |
| Red `✗`  | Color("9")  | Connection failed |

### Channel-driven real-time updates

Status updates are pushed from the Manager to the TUI via a Go channel, reusing the same pattern as the SandboxBar:

```
Manager.sendNotify()
    → mcpNotifyCh <- statuses (an 8-slot buffered channel in main.go)
    → waitMCPUpdate(mcpCh) tea.Cmd (blocking read on the channel)
    → mcpUpdateMsg{statuses}
    → Update() case mcpUpdateMsg: m.mcpServers = msg.statuses
    → View() renderMCPBar() / renderMCPPanel()
```

`mcpNotifyCh` uses an 8-slot buffered channel with `select { default: }` non-blocking sends — the Manager goroutine never blocks due to TUI back-pressure, at most dropping one intermediate status update (the next state change will refresh it).

### `/mcp` tool panel

Typing `/mcp` (with Tab completion support) opens a modal tool panel showing the full tool list:

```
MCP Tool Management

● context7  2 tools
  · mcp__context7__resolve_library_id
    Resolves a library or package name to a Context7-compatible library ID
  · mcp__context7__get_library_docs
    Fetches up-to-date documentation for a library

e edit config  ↑↓/jk scroll  Esc close
```

**Keyboard shortcuts:**

| Key | Action |
|----|------|
| `e` / `E` | Suspend the TUI (`tea.ExecProcess`) and open the `.mcp.json` config file |
| `↑` / `k` | Scroll the tool list up |
| `↓` / `j` | Scroll the tool list down |
| `Esc` | Close the panel |
| `Ctrl+C` | Exit the program |

**Editor-opening logic:**

```go
func openMCPConfigCmd(path string) tea.Cmd {
    editor := os.Getenv("EDITOR")
    if editor != "" {
        // when $EDITOR is set (vim/nano/etc): suspend the TUI, the editor fully takes over the terminal, TUI resumes on exit
        return tea.ExecProcess(exec.Command(editor, path), func(err error) tea.Msg {
            return mcpEditorDoneMsg{err: err}
        })
    }
    // when $EDITOR is not set: use `open` on macOS, `xdg-open` on Linux (opens in the background, TUI resumes immediately)
    openCmd := "open"
    if runtime.GOOS == "linux" { openCmd = "xdg-open" }
    return tea.ExecProcess(exec.Command(openCmd, path), func(err error) tea.Msg {
        return mcpEditorDoneMsg{err: err}
    })
}
```

`tea.ExecProcess` is a Bubbletea API specifically designed for integrating external programs: when invoked, the TUI proactively releases terminal control (`p.ReleaseTerminal()`) and reclaims it after the external program exits (`p.RestoreTerminal()`). This allows terminal editors (vim/nano) to fully take over the screen, with the TUI seamlessly resuming once the user finishes editing.

---

## Comparison with Mainstream Frameworks

harness9's MCP implementation draws on the design of 6 mainstream Agent Harness frameworks. The core decisions are as follows:

| Dimension | harness9's choice | Reference framework |
|------|--------------|---------|
| Tool integration model | **Adapter injected into the Registry** (MCPToolAdapter) | OpenHarness (McpToolAdapter) |
| Naming convention | `mcp__{server}__{tool}` **double underscore** | Claude Agent SDK, OpenHarness |
| Server lifecycle | **Session-scoped** (connected at main.go startup, stopped on exit) | OpenHarness (connect_all/close) |
| Connection strategy | **Concurrent fail-soft** (an independent goroutine per server, failures don't block others) | OpenHarness (concurrent connect_all) |
| Transport layer | **stdio + HTTP** (interface abstraction, two implementations) | OpenCode (stdio + HTTP + SSE) |
| Large tool sets | No special optimization currently | Claude Agent SDK (Tool Search, ≤10,000 tools) |

**Why Adapter instead of lazy merge**: harness9's Registry is the single entry point on the Engine's call path. The Adapter pattern lets MCP tools "become" harness9 tools at registration time, requiring no MCP-specific handling at LLM call time. Compared to OpenCode's `MCP.tools()` lazy-merge approach, the benefit of the Adapter pattern is that tool registration failures (e.g. naming conflicts) are discovered at startup, rather than being exposed only at LLM call time.

---

## Debugging and Common Issues

**Server connection failure (MCPBar shows red `✗`)**

Check the `[mcp]`-prefixed lines in the startup logs:
```bash
go run ./cmd/harness9 2>&1 | grep '\[mcp\]'
# [mcp] server "context7" connect failed: start process npx: ...
```

Common causes:
- `npx` is not installed or not on PATH
- Network issues (HTTP Server unreachable)
- Incorrect MCP Server package name (check `args`)

**Tools injected but the LLM doesn't call them**

The description of an MCP tool comes directly from the MCP Server's `tools/list` response, and quality varies. If the LLM doesn't call a particular tool, use the `/mcp` panel to inspect the tool's description and confirm whether it is clear enough.

**Taking effect immediately after editing the config**

In the current version, changes to `.mcp.json` require restarting harness9 to take effect (server connections are established at startup). Hot reload (reconnecting at runtime) is a future extension direction, with the integration point at the `Manager.Reconnect` method (not currently exposed to the TUI).
