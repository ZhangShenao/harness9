// Package engine — loopGuard 守护对象（方案 B）。
//
// 设计要点（spec §4.4-§4.7）：
//   - 集中持有全部护栏状态，runLoop 只在固定检查点调用，不散落 if 块
//   - 硬熔断只在 Turn 边界裁决，接受最多一轮的过冲，绝不在流式中途撕断
//   - 所有受控终止收敛到 Terminated() reason + 统一出口（由 runLoop 的
//     terminate 闭包承接），修复旧实现熔断路径跳过历史持久化的缺陷
package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/harness9/internal/schema"
)

// turnSignature 是单条工具调用的身份签名（name + canonical args 的 SHA256）。
type turnSignature [sha256.Size]byte

// GuardConfig 汇总全部护栏配置。零值字段一律表示该护栏关闭——这保证
// 未开启任何新选项的引擎行为与引入护栏前完全一致（向后兼容硬约束）。
type GuardConfig struct {
	MaxTurns            int           // <=0 关闭
	RunTimeout          time.Duration // <=0 关闭
	TokenBudget         int           // <=0 关闭
	RepetitionWindow    int           // <=0 关闭
	RepetitionThreshold int
	StallWindow         int // <=0 关闭
	StallText           string
	MemoryInterval      int // <=0 关闭
	MemoryText          string
}

// GuardTermination 是 guard 已作出的终止裁决结论。
type GuardTermination struct {
	Reason TerminationReason
}

// loopGuard 集中持有单次 runLoop 的全部护栏状态。生命周期与一次 runLoop
// 相同，非并发安全（只在 runLoop 主 goroutine 上访问）。
type loopGuard struct {
	cfg        GuardConfig
	start      time.Time
	deadline   time.Time // 零值 = 无墙钟限制
	terminated *GuardTermination

	inputTokens int // 累计 input tokens（实际优先，缺失时估算兜底）

	repWindow    []sigEntry // 最近 repWindow 个 turn 的签名记录（滑动窗口）
	reminded     bool       // 本轮 Run 内是否已注入过重复提醒（升级判据）
	lastHitTotal int        // 最近一次命中的相同签名总次数（用于文案）
	sigLabel     map[turnSignature]string
	repTotal     map[turnSignature]int

	turnsSinceProgress int
}

// sigEntry 记录单个 turn 内出现的签名及出现次数。
type sigEntry struct {
	turn   int
	counts map[turnSignature]int
}

// newLoopGuard 构造守护对象。start 为该次 runLoop 的起始时刻。
func newLoopGuard(cfg GuardConfig, start time.Time) *loopGuard {
	g := &loopGuard{cfg: cfg, start: start, repTotal: make(map[turnSignature]int), sigLabel: make(map[turnSignature]string)}
	if cfg.RunTimeout > 0 {
		g.deadline = start.Add(cfg.RunTimeout)
	}
	return g
}

// computeSignature 计算"工具名 + canonical 化参数"的签名。
// canonical 化经 map[string]any 往返序列化，消除空白与键序差异；
// 参数不是合法 JSON 时退化为原始字节哈希（fail-open，不因畸形参数阻塞循环）。
func computeSignature(tc schema.ToolCall) turnSignature {
	h := sha256.New()
	h.Write([]byte(tc.Name))
	args := tc.Arguments
	var v any
	if err := json.Unmarshal(args, &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			args = b
		}
	}
	h.Write(args)
	sig := turnSignature{}
	copy(sig[:], h.Sum(nil))
	return sig
}

// CheckTurn 在 Turn 边界裁决轮数与墙钟。返回非 nil error 表示受控终止，
// 调用方应立即以统一出口退出循环。
func (g *loopGuard) CheckTurn(turnCount int) error {
	if g.terminated != nil {
		return g.alreadyErr()
	}
	if !g.deadline.IsZero() && time.Now().After(g.deadline) {
		g.terminated = &GuardTermination{Reason: ReasonRunTimeout}
		return fmt.Errorf("已达运行超时上限 (%s)，循环终止", g.cfg.RunTimeout)
	}
	if g.cfg.MaxTurns > 0 && turnCount > g.cfg.MaxTurns {
		g.terminated = &GuardTermination{Reason: ReasonMaxTurns}
		// 措辞与旧实现逐字一致：既有测试以 strings.Contains 断言。
		return fmt.Errorf("已达最大 Turn 数 (%d)，循环终止", g.cfg.MaxTurns)
	}
	return nil
}

// CheckBudget 在 Turn 边界裁决累计 token 预算。
func (g *loopGuard) CheckBudget() error {
	if g.terminated != nil {
		return g.alreadyErr()
	}
	if g.cfg.TokenBudget > 0 && g.inputTokens >= g.cfg.TokenBudget {
		g.terminated = &GuardTermination{Reason: ReasonTokenBudget}
		return fmt.Errorf("已达到 Token 预算 (%d)，循环终止", g.cfg.TokenBudget)
	}
	return nil
}

// AddUsage 累计 input token 用量。usage 非 nil 且 InputTokens>0 时取实际值；
// 否则以 fallbackEstimate（发送给 LLM 的上下文估算值）兜底，保证无 usage 的
// Provider 也有预算约束。
func (g *loopGuard) AddUsage(usage *schema.Usage, fallbackEstimate int) {
	switch {
	case usage != nil && usage.InputTokens > 0:
		g.inputTokens += usage.InputTokens
	case fallbackEstimate > 0:
		g.inputTokens += fallbackEstimate
	}
}

// Remaining 返回剩余墙钟时间。deadline 未激活时 ok=false。
// 供工具子 context 计算 min(toolTimeout, remaining)，防止单个长工具冲破 deadline。
func (g *loopGuard) Remaining() (time.Duration, bool) {
	if g.deadline.IsZero() {
		return 0, false
	}
	d := time.Until(g.deadline)
	if d < 0 {
		d = 0
	}
	return d, true
}

// Terminated 返回已作出的终止裁决。
func (g *loopGuard) Terminated() (TerminationReason, bool) {
	if g.terminated == nil {
		return "", false
	}
	return g.terminated.Reason, true
}

// alreadyErr 在裁决已作出后的重复检查中返回一致错误。
func (g *loopGuard) alreadyErr() error {
	reason, _ := g.Terminated()
	return fmt.Errorf("guard 已裁决终止 (%s)", reason)
}
