// Package ltm — Store：长期记忆的 SQLite 数据访问层。
// 本文件实现 Store，管理 long_term_memories 表与 standalone FTS5 memories_fts 索引，
// 提供去重写入（Add）、FTS5 全文检索（Search）、更新（Update）、软删除（SoftDelete）、
// 列表（List）、清理（PurgeExpired）和陈旧识别（StaleCandidates）等操作。
package ltm

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound 表示按 ID 查询的记忆不存在。
var ErrNotFound = errors.New("记忆不存在")

const ltmSchema = `
CREATE TABLE IF NOT EXISTS long_term_memories (
    id           TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL,
    category     TEXT,
    importance   INTEGER NOT NULL DEFAULT 0,
    signature    TEXT UNIQUE,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    use_count    INTEGER NOT NULL DEFAULT 0,
    ttl_days     INTEGER,
    disabled     INTEGER NOT NULL DEFAULT 0,
    tags         TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id UNINDEXED, title, content);
`

// Store 是长期记忆的 SQLite 数据访问层，复用 Manager 持有的 *sql.DB 连接。
// long_term_memories 表为唯一事实源；memories_fts 为手动同步的 standalone FTS5 索引。
type Store struct {
	db  *sql.DB
	now func() time.Time // 可注入，便于测试确定性
}

// NewStore 在给定连接上初始化 LTM schema（幂等），返回 Store。
func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(ltmSchema); err != nil {
		return nil, fmt.Errorf("初始化 ltm schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// 全部列，供 scanEntry 复用。
const entryColumns = `id, title, content, category, importance, signature, created_at, updated_at, last_used_at, use_count, ttl_days, disabled, tags`

type scanner interface{ Scan(dest ...any) error }

func scanEntry(sc scanner) (*Entry, error) {
	var e Entry
	var category, signature, tags sql.NullString
	var createdAt, updatedAt int64
	var lastUsed, ttlDays sql.NullInt64
	var disabled int
	if err := sc.Scan(&e.ID, &e.Title, &e.Content, &category, &e.Importance, &signature,
		&createdAt, &updatedAt, &lastUsed, &e.UseCount, &ttlDays, &disabled, &tags); err != nil {
		return nil, err
	}
	e.Category = Category(category.String)
	e.Signature = signature.String
	e.CreatedAt = time.Unix(createdAt, 0)
	e.UpdatedAt = time.Unix(updatedAt, 0)
	if lastUsed.Valid {
		e.LastUsedAt = time.Unix(lastUsed.Int64, 0)
	}
	if ttlDays.Valid {
		e.TTLDays = int(ttlDays.Int64)
	}
	e.Disabled = disabled != 0
	if tags.String != "" {
		_ = json.Unmarshal([]byte(tags.String), &e.Tags)
	}
	return &e, nil
}

// nullTTL 将 0 映射为 NULL（永不过期），否则返回天数。
func nullTTL(days int) any {
	if days <= 0 {
		return nil
	}
	return days
}

func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Add 写入一条新记忆。内容签名已存在（未删除）时视为去重命中：
// 刷新 updated_at + 自增 use_count，返回既有条目，不插入新行。
func (s *Store) Add(ctx context.Context, e *Entry) (*Entry, error) {
	sig := Signature(e.Content)
	now := s.now().Unix()

	var existingID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM long_term_memories WHERE signature = ? AND disabled = 0`, sig).Scan(&existingID)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE long_term_memories SET updated_at = ?, use_count = use_count + 1 WHERE id = ?`,
			now, existingID); err != nil {
			return nil, fmt.Errorf("去重刷新: %w", err)
		}
		return s.Get(ctx, existingID)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("查询签名: %w", err)
	}

	id := newID()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO long_term_memories
			(id, title, content, category, importance, signature, created_at, updated_at, use_count, ttl_days, disabled, tags)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?)`,
		id, e.Title, e.Content, string(e.Category), e.Importance, sig, now, now, nullTTL(e.TTLDays), marshalTags(e.Tags)); err != nil {
		return nil, fmt.Errorf("插入记忆: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories_fts (id, title, content) VALUES (?, ?, ?)`, id, e.Title, e.Content); err != nil {
		return nil, fmt.Errorf("插入 fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交事务: %w", err)
	}
	return s.Get(ctx, id)
}

// ftsQuery 把用户查询转换为安全的 FTS5 MATCH 表达式：
// 按空白分词，每个 token 作为双引号短语（内部双引号翻倍转义），以 OR 连接。
// 无有效 token 时返回空串。
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// Search 用 FTS5 检索未删除、未过期的记忆，按相关度返回至多 limit 条。
// 命中条目执行强化：自增 use_count、更新 last_used_at。
func (s *Store) Search(ctx context.Context, query string, limit int) ([]*Entry, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM memories_fts WHERE memories_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts 检索: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描 fts 结果: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 fts 结果: %w", err)
	}

	now := s.now()
	var result []*Entry
	for _, id := range ids {
		e, err := s.Get(ctx, id)
		if err != nil {
			continue // 容忍并发删除
		}
		if e.Disabled || e.Expired(now) {
			continue
		}
		// 强化命中条目。
		if _, err := s.db.ExecContext(ctx,
			`UPDATE long_term_memories SET use_count = use_count + 1, last_used_at = ? WHERE id = ?`,
			now.Unix(), id); err != nil {
			return nil, fmt.Errorf("强化命中: %w", err)
		}
		e.UseCount++
		e.LastUsedAt = now
		result = append(result, e)
	}
	return result, nil
}

// Update 按 e.ID 更新标题/内容/分类/重要度/TTL/标签，重算签名并刷新 updated_at，
// 同步重建该条目的 FTS 索引。条目不存在时返回错误。
func (s *Store) Update(ctx context.Context, e *Entry) error {
	sig := Signature(e.Content)
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx,
		`UPDATE long_term_memories
		 SET title = ?, content = ?, category = ?, importance = ?, signature = ?, ttl_days = ?, tags = ?, updated_at = ?
		 WHERE id = ?`,
		e.Title, e.Content, string(e.Category), e.Importance, sig, nullTTL(e.TTLDays), marshalTags(e.Tags), now, e.ID)
	if err != nil {
		return fmt.Errorf("更新记忆: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, e.ID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE id = ?`, e.ID); err != nil {
		return fmt.Errorf("清理 fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories_fts (id, title, content) VALUES (?, ?, ?)`, e.ID, e.Title, e.Content); err != nil {
		return fmt.Errorf("重建 fts: %w", err)
	}
	return tx.Commit()
}

// SoftDelete 将条目标记为 disabled（不物理删除，保留审计），并移出 FTS 索引。
// 同时将 signature 置为 NULL，释放唯一约束槽位，使相同内容可在未来重新被添加。
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx,
		`UPDATE long_term_memories SET disabled = 1, signature = NULL, updated_at = ? WHERE id = ?`, s.now().Unix(), id)
	if err != nil {
		return fmt.Errorf("软删除: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("清理 fts: %w", err)
	}
	return tx.Commit()
}

// queryEntries 是 List/StaleCandidates 共享的多行查询执行器。
func (s *Store) queryEntries(ctx context.Context, where string, args ...any) ([]*Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM long_term_memories WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("查询记忆列表: %w", err)
	}
	defer rows.Close()
	var result []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描记忆: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// List 返回未删除、未过期的记忆，按 importance 降序、updated_at 降序排列，至多 limit 条。
// 供 Precis 渲染精华视图使用。limit<=0 时不限制。
func (s *Store) List(ctx context.Context, limit int) ([]*Entry, error) {
	nowUnix := s.now().Unix()
	where := `disabled = 0 AND (ttl_days IS NULL OR updated_at + ttl_days * 86400 >= ?)
		ORDER BY importance DESC, updated_at DESC`
	if limit > 0 {
		where += fmt.Sprintf(" LIMIT %d", limit)
	}
	return s.queryEntries(ctx, where, nowUnix)
}

// PurgeExpired 将所有已过 TTL 的未删除记忆软删除，返回回收条数。
// 同时将 signature 置为 NULL（与 SoftDelete 一致），释放唯一约束槽位，
// 使相同内容在未来可以重新被添加（否则 Add 的签名去重会将其视为既有记录）。
func (s *Store) PurgeExpired(ctx context.Context) (int, error) {
	nowUnix := s.now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE long_term_memories SET disabled = 1, signature = NULL
		 WHERE disabled = 0 AND ttl_days IS NOT NULL AND updated_at + ttl_days * 86400 < ?`, nowUnix)
	if err != nil {
		return 0, fmt.Errorf("回收过期记忆: %w", err)
	}
	// 同步清理 FTS：仅删除本次刚被回收的条目（与 UPDATE 条件保持一致，避免扫描全量历史软删除行）。
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM memories_fts WHERE id IN (
			SELECT id FROM long_term_memories
			WHERE disabled = 1 AND signature IS NULL AND ttl_days IS NOT NULL
			  AND updated_at + ttl_days * 86400 < ?
		)`, nowUnix); err != nil {
		return 0, fmt.Errorf("清理 fts: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// StaleCandidates 识别清理候选：importance<=1 且 use_count=0 且 60 天未更新的未删除条目。
// 当前为 Phase 3 接缝——供未来的 Consolidator（Dreaming 巩固）消费，主流程暂未调用。
func (s *Store) StaleCandidates(ctx context.Context) ([]*Entry, error) {
	cutoff := s.now().Add(-60 * 24 * time.Hour).Unix()
	return s.queryEntries(ctx,
		`disabled = 0 AND importance <= 1 AND use_count = 0 AND updated_at < ?`, cutoff)
}

// Get 按 ID 返回条目（含已软删除的，便于审计）。不存在时返回错误。
func (s *Store) Get(ctx context.Context, id string) (*Entry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM long_term_memories WHERE id = ?`, id)
	e, err := scanEntry(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询记忆: %w", err)
	}
	return e, nil
}
