// Package ltm 实现 harness9 的长期记忆（Long-Term Memory）能力：
// 跨会话持久化的知识/偏好/任务/技能条目。SQLite long_term_memories 表为唯一事实源，
// MEMORY.md 物化视图注入 System Prompt，FTS5 提供按需全文检索。
package ltm

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Category 是长期记忆条目的分类，影响精华渲染与检索语义。
type Category string

const (
	CategoryKnowledge  Category = "knowledge"  // 事实性知识
	CategoryPreference Category = "preference" // 用户偏好
	CategoryTask       Category = "task"       // 跨会话任务/承诺
	CategorySkill      Category = "skill"      // 操作性技能/方法
)

// Entry 是一条长期记忆。TTLDays ≤ 0 表示永不过期；Disabled 为软删除标志。
type Entry struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	Category   Category  `json:"category,omitempty"`
	Importance int       `json:"importance"` // 0-10，决定精华排序与陈旧识别
	Signature  string    `json:"-"`          // SHA256(normalize(content))，用于去重
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	UseCount   int       `json:"use_count"`
	TTLDays    int       `json:"ttl_days,omitempty"`
	Disabled   bool      `json:"-"`
	Tags       []string  `json:"tags,omitempty"`
}

// Signature 计算内容的去重指纹：SHA256(normalize(content))。
func Signature(content string) string {
	sum := sha256.Sum256([]byte(normalize(content)))
	return hex.EncodeToString(sum[:])
}

// normalize 折叠空白、小写化、去除首尾空白，得到稳定的去重基准串。
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Expired 报告条目相对 now 是否已超过 TTL。TTLDays<=0 视为永不过期。
func (e Entry) Expired(now time.Time) bool {
	if e.TTLDays <= 0 {
		return false
	}
	return e.UpdatedAt.Add(time.Duration(e.TTLDays) * 24 * time.Hour).Before(now)
}
