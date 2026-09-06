// Package memory — Manager：SQLite 数据库连接持有者与会话生命周期管理。
// 本文件实现 Manager 类型，整个进程共享一个实例，负责会话的创建、查询、删除以及级联 GC。
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/harness9/internal/logfmt"

	_ "modernc.org/sqlite"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    PRIMARY KEY,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL,
    role         TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    tool_calls   TEXT,
    tool_call_id TEXT,
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);

CREATE TABLE IF NOT EXISTS session_plans (
    session_id TEXT PRIMARY KEY,
    items      TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
`

// Manager 持有共享 SQLite 连接，管理所有会话的生命周期。
// 整个进程共享一个 Manager 实例。
type Manager struct {
	db                   *sql.DB
	toolResultsDir       string // 可选，非空时 DeleteSession 级联清理
	compactionRecordsDir string // 可选，非空时 DeleteSession 级联清理
}

// ManagerOption 配置 Manager 的可选行为。
type ManagerOption func(*Manager)

// WithToolResultsDir 设置工具输出 offload 文件的根目录。
// 设置后，DeleteSession 会删除对应 session 的 offload 子目录。
func WithToolResultsDir(dir string) ManagerOption {
	return func(m *Manager) { m.toolResultsDir = dir }
}

// WithCompactionRecordsDir 设置压缩记录 JSONL 文件的根目录。
// 设置后，DeleteSession 会删除对应 session 的压缩记录文件。
func WithCompactionRecordsDir(dir string) ManagerOption {
	return func(m *Manager) { m.compactionRecordsDir = dir }
}

// NewManager 打开（或创建）指定路径的 SQLite 数据库，初始化 Schema。
// 父目录不存在时自动创建。
func NewManager(dbPath string, opts ...ManagerOption) (*Manager, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("创建数据库目录：%w", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库：%w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("设置 pragma: %w", err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 schema: %w", err)
	}
	m := &Manager{db: db}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// NewSession 生成 UUID，在数据库中创建新会话记录，返回绑定该会话的 Session。
func (m *Manager) NewSession(ctx context.Context) (Session, error) {
	id := newUUID()
	now := time.Now().Unix()
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at) VALUES (?, ?, ?)`,
		id, now, now)
	if err != nil {
		return nil, fmt.Errorf("创建会话：%w", err)
	}
	return &SQLiteSession{db: m.db, sessionID: id}, nil
}

// OpenSession 打开已有会话；session 不存在时返回错误。
func (m *Manager) OpenSession(ctx context.Context, id string) (Session, error) {
	var count int
	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("查询会话：%w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("会话不存在: %s", id)
	}
	return &SQLiteSession{db: m.db, sessionID: id}, nil
}

// ListSessions 按 updated_at 降序返回所有会话元数据（含消息条数）。
func (m *Manager) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id, s.created_at, s.updated_at, COUNT(msg.id)
		FROM sessions s
		LEFT JOIN messages msg ON msg.session_id = s.id
		GROUP BY s.id
		ORDER BY s.updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("列出会话：%w", err)
	}
	defer rows.Close()

	var result []SessionInfo
	for rows.Next() {
		var info SessionInfo
		var createdAt, updatedAt int64
		if err := rows.Scan(&info.ID, &createdAt, &updatedAt, &info.MsgCount); err != nil {
			return nil, fmt.Errorf("扫描会话：%w", err)
		}
		info.CreatedAt = time.Unix(createdAt, 0)
		info.UpdatedAt = time.Unix(updatedAt, 0)
		result = append(result, info)
	}
	return result, rows.Err()
}

// DeleteSession 删除指定会话及其所有消息（通过 ON DELETE CASCADE）。
// 若设置了 toolResultsDir，还会级联清理对应 session 的 offload 子目录。
// 若设置了 compactionRecordsDir，还会级联清理对应 session 的压缩记录文件。
// 级联文件清理为 fail-soft：失败仅记录日志，不影响会话删除结果。
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除会话：%w", err)
	}
	if m.toolResultsDir != "" {
		if rmErr := os.RemoveAll(filepath.Join(m.toolResultsDir, id)); rmErr != nil {
			log.Print(logfmt.FormatMsg("memory", fmt.Sprintf("清理会话 offload 目录失败: %v", rmErr)))
		}
	}
	if m.compactionRecordsDir != "" {
		if rmErr := os.Remove(filepath.Join(m.compactionRecordsDir, id+".jsonl")); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Print(logfmt.FormatMsg("memory", fmt.Sprintf("清理会话压缩记录失败: %v", rmErr)))
		}
	}
	return nil
}

// Close 关闭数据库连接。
func (m *Manager) Close() error {
	return m.db.Close()
}

// DB 返回底层 SQLite 连接，供长期记忆（ltm.Store）等共享同一连接复用。
// 调用方不得关闭该连接——其生命周期由 Manager.Close 统一管理。
func (m *Manager) DB() *sql.DB {
	return m.db
}

// newUUID 生成随机 UUID v4。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
