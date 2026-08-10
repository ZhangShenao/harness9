// Package memory - CompactionRecord 与 FileRecordStore：ProgressiveCompactor 的压缩记录持久化。
// 本文件定义 CompactionTier 枚举（分级压缩触发档位）、CompactionRecord 结构体（单次压缩的完整审计记录）、
// RecordStore 接口与 FileRecordStore（JSONL 文件持久化实现，每个 session 一个 <sessionID>.jsonl 文件）。
// CompactionRecord 携带锚点、外存条目、压缩比等元数据，供后续轮次回溯压缩历史与调试分析。
package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// CompactionTier 标识触发压缩的档位，由 ProgressiveCompactor 按上下文占用比例分级。
type CompactionTier int

const (
	// TierNone 未触发压缩。
	TierNone CompactionTier = iota
	// TierWarn 预警档：接近阈值，仅做轻量整理。
	TierWarn
	// TierSoft 软压缩：摘要 + 保留较多尾部消息。
	TierSoft
	// TierFull 全量压缩：摘要 + 锚点 + 外存 + 最小尾部保留。
	TierFull
	// TierEmergency 紧急压缩：上下文接近硬上限，激进截断保命。
	TierEmergency
)

// CompactionRecord 记录单次压缩的完整审计信息，由 RecordStore 持久化。
type CompactionRecord struct {
	ID               string         `json:"id"`
	SessionID        string         `json:"session_id"`
	Timestamp        time.Time      `json:"timestamp"`
	Tier             CompactionTier `json:"tier"`
	TokensBefore     int            `json:"tokens_before"`
	TokensAfter      int            `json:"tokens_after"`
	MsgsBefore       int            `json:"msgs_before"`
	MsgsAfter        int            `json:"msgs_after"`
	Anchors          []Anchor       `json:"anchors"`
	Offloaded        []OffloadEntry `json:"offloaded"`
	Summarized       int            `json:"summarized"`
	PreservedTail    int            `json:"preserved_tail"`
	SummaryText      string         `json:"summary_text"`
	CompressionRatio float64        `json:"compression_ratio"`
	Duration         time.Duration  `json:"duration"`
	Error            string         `json:"error,omitempty"`
}

// FillDefaults 填充记录的默认值：缺失时间戳补当前时间，并在 TokensBefore>0 时计算压缩比。
func (r *CompactionRecord) FillDefaults() {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	if r.TokensBefore > 0 {
		r.CompressionRatio = float64(r.TokensAfter) / float64(r.TokensBefore)
	}
}

// RecordStore 持久化压缩记录并按 session 检索。FileRecordStore 为默认文件实现。
type RecordStore interface {
	Append(record CompactionRecord) error
	List(sessionID string) ([]CompactionRecord, error)
}

// FileRecordStore 将压缩记录以 JSONL 形式持久化到 dir 目录，
// 每个 session 对应一个 <sessionID>.jsonl 文件，追加写入、顺序读取。
type FileRecordStore struct {
	dir string
}

// NewFileRecordStore 构造一个以 dir 为存储根目录的 FileRecordStore。
func NewFileRecordStore(dir string) *FileRecordStore {
	return &FileRecordStore{dir: dir}
}

// Append 将单条压缩记录追加到对应 session 的 JSONL 文件，必要时创建目录与文件。
func (s *FileRecordStore) Append(record CompactionRecord) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, record.SessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(record)
}

// List 读取指定 session 的全部压缩记录，按写入顺序返回。
// 文件不存在时返回 nil, nil（视为空 session，非错误）。
func (s *FileRecordStore) List(sessionID string) ([]CompactionRecord, error) {
	path := filepath.Join(s.dir, sessionID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var records []CompactionRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var r CompactionRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, scanner.Err()
}
