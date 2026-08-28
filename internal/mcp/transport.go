package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioCloseTimeout 是 Close() 等待 readLoop 退出的最长时限。
// 理论上进程组已被 kill、stdout 已关闭时 readLoop 应立即退出；
// 此超时作为最终兜底，防止极端环境下进程残留导致永久阻塞。
const stdioCloseTimeout = 3 * time.Second

// Transport 抽象了与 MCP Server 的底层消息通道。
// 实现者负责：建立连接、发送 JSON-RPC 请求并等待响应、发送通知（无需等待响应）、关闭连接。
type Transport interface {
	// Start 初始化传输层（如启动子进程）。返回 error 表示连接失败。
	Start(ctx context.Context) error
	// Send 发送 JSON-RPC 方法调用并阻塞等待响应（ctx 超时或取消时提前返回）。
	Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	// Notify 发送 JSON-RPC 通知（无 ID，不等待响应）。
	Notify(method string, params json.RawMessage) error
	// Close 关闭传输层并释放资源。
	Close() error
}

// rpcRequest 是 JSON-RPC 2.0 请求结构体。ID 为 nil 时表示通知（Notification）。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse 是 JSON-RPC 2.0 响应结构体。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 是 JSON-RPC 2.0 错误对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// StdioTransport 通过子进程的 stdin/stdout 实现 JSON-RPC 2.0 传输。
// 每条消息占一行（NDJSON），通过 ID 关联请求与响应。
type StdioTransport struct {
	command string
	args    []string
	env     []string // 格式：["KEY=VALUE", ...]

	cmd     *exec.Cmd
	enc     *json.Encoder // 写入 stdin（由 writeMu 保护）
	scan    *bufio.Scanner
	loopRun bool // readLoop goroutine 是否已启动（Close 据此决定是否等待 done）

	writeMu sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	done    chan struct{}
}

// NewStdioTransport 创建 StdioTransport。command/args 指定子进程命令；env 为额外环境变量。
func NewStdioTransport(command string, args, env []string) *StdioTransport {
	return &StdioTransport{
		command: command,
		args:    args,
		env:     env,
		pending: make(map[int64]chan rpcResponse),
		done:    make(chan struct{}),
	}
}

// Start 启动子进程并开始读取 stdout 中的响应。
func (t *StdioTransport) Start(ctx context.Context) error {
	t.cmd = exec.CommandContext(ctx, t.command, t.args...)
	// 独立进程组：使子进程（如 npx）及其所有后代（如 node）归属于同一进程组。
	// Close() 通过 killProcessGroup 一次性终止整个组，避免 node 孤儿进程继续持有
	// stdout pipe 导致 readLoop.Scan() 永久阻塞。平台相关实现见 transport_proc_*.go。
	setPgid(t.cmd)
	if len(t.env) > 0 {
		t.cmd.Env = append(t.cmd.Environ(), t.env...)
	}

	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// stderr 静默丢弃，避免污染 TUI 输出。
	t.cmd.Stderr = io.Discard

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("start process %s: %w", t.command, err)
	}

	t.enc = json.NewEncoder(stdin)
	t.scan = bufio.NewScanner(stdout)
	t.scan.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB 缓冲，适应大工具列表响应

	t.loopRun = true
	go t.readLoop()
	return nil
}

// readLoop 在后台持续读取子进程 stdout，按 ID 将响应路由到对应的 pending channel。
func (t *StdioTransport) readLoop() {
	defer close(t.done)
	for t.scan.Scan() {
		line := t.scan.Bytes()
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // 忽略无法解析的行（如启动日志）
		}
		if resp.ID == nil {
			continue // 通知消息，忽略
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
	// readLoop 结束：通知所有等待中的请求失败。
	// ch 均以 make(chan rpcResponse, 1) 创建（容量 1），此处在持锁下发送不会阻塞，
	// 无死锁风险——Send 端的 select 三路均不需要先获取 t.mu。
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, ch := range t.pending {
		ch <- rpcResponse{Error: &rpcError{Message: "transport closed"}}
		delete(t.pending, id)
	}
}

// Send 发送 JSON-RPC 请求并等待响应（支持 ctx 超时取消）。
func (t *StdioTransport) Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	ch := make(chan rpcResponse, 1)

	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}

	t.writeMu.Lock()
	err := t.enc.Encode(req)
	t.writeMu.Unlock()
	if err != nil {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, fmt.Errorf("encode request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("transport closed")
	}
}

// Notify 发送 JSON-RPC 通知（无 ID，不等待响应）。
func (t *StdioTransport) Notify(method string, params json.RawMessage) error {
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.enc.Encode(req)
}

// Close 终止子进程并等待 readLoop goroutine 退出。
// 若 Start() 在启动 readLoop 前失败（loopRun == false），跳过 <-t.done 以避免永久阻塞。
func (t *StdioTransport) Close() error {
	if t.cmd != nil && t.cmd.Process != nil {
		pid := t.cmd.Process.Pid
		// 优先杀整个进程组（平台相关实现见 transport_proc_*.go），确保 npx fork 出的
		// node 子进程也被终止；回退到杀单个进程（跨平台兜底）。
		if err := killProcessGroup(pid); err != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.cmd.Wait()
	}
	if t.loopRun {
		select {
		case <-t.done:
		case <-time.After(stdioCloseTimeout):
		}
	}
	return nil
}

// HTTPTransport 通过 HTTP POST 实现 JSON-RPC 2.0 传输（Streamable HTTP）。
// 每次 Send 调用是一个独立的 HTTP 请求，无连接状态。
type HTTPTransport struct {
	url     string
	headers map[string]string
	nextID  atomic.Int64
}

// NewHTTPTransport 创建 HTTPTransport。
func NewHTTPTransport(url string, headers map[string]string) *HTTPTransport {
	return &HTTPTransport{url: url, headers: headers}
}

// Start 对 HTTP 传输无需特殊初始化（HTTP 是无状态的）。
func (t *HTTPTransport) Start(_ context.Context) error { return nil }

// Send 发送 HTTP POST JSON-RPC 请求并等待响应。
func (t *HTTPTransport) Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// Notify 通过 HTTP POST 发送 JSON-RPC 通知（fire-and-forget，忽略响应体）。
// 使用 5s 超时避免服务器不可达时无限阻塞。
// 关闭前限量 drain 响应体，使底层 TCP 连接可被 keep-alive 复用。
func (t *HTTPTransport) Notify(method string, params json.RawMessage) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// Close 对 HTTP 传输无需额外清理。
func (t *HTTPTransport) Close() error { return nil }
