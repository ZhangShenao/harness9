// ApprovalModal.tsx — 审批弹窗：五选项语义与 TUI 对齐
//（允许一次 / 本次会话 / 总是允许 / 拒绝 / 拒绝并反馈）。
// appservice 的泵不阻塞，多审批排队展示，逐个处理。
import { useState } from "react";
import { useStore } from "../store";

export function ApprovalModal() {
  const approvals = useStore((s) => s.approvals);
  const respond = useStore((s) => s.respondApproval);
  const [feedback, setFeedback] = useState("");
  const [showFeedback, setShowFeedback] = useState(false);

  const current = approvals[0];
  if (!current) return null;
  const riskCls = current.risk_level === "high" ? "risk-high" : current.risk_level === "medium" ? "risk-medium" : "risk-low";

  const act = (approved: boolean, remember: boolean, withFeedback?: string) => {
    respond(current.id, approved, withFeedback ?? "", remember).catch(() => {});
    setFeedback("");
    setShowFeedback(false);
  };

  return (
    <div className="modal-mask">
      <div className={`approval-card ${riskCls}`}>
        <div className="approval-head">
          <span className="approval-icon">🔐</span>
          <div>
            <div className="approval-title">需要你的批准</div>
            <div className="dim small">
              工具 <code>{current.tool_name}</code> · 风险等级{" "}
              <span className={`risk-tag ${riskCls}`}>{current.risk_level}</span>
              （队列中还有 {approvals.length - 1} 个）
            </div>
          </div>
        </div>

        {current.reason && <div className="approval-reason">{current.reason}</div>}
        {current.args && (
          <pre className="approval-args">{(() => {
            try {
              return JSON.stringify(JSON.parse(current.args), null, 2);
            } catch {
              return current.args;
            }
          })()}</pre>
        )}

        {showFeedback && (
          <textarea
            className="feedback-input"
            placeholder="告诉 Agent 为什么拒绝、应该怎么做…"
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            autoFocus
          />
        )}

        <div className="approval-actions">
          <button className="btn btn-ok" onClick={() => act(true, false)}>
            ✓ 允许一次
          </button>
          <button className="btn btn-ok" onClick={() => act(true, true)}>
            ✓✓ 总是允许
          </button>
          <button className="btn btn-danger" onClick={() => act(false, false)}>
            ✗ 拒绝
          </button>
          <button
            className="btn btn-ghost"
            onClick={() => {
              if (showFeedback && feedback.trim()) {
                act(false, false, feedback.trim());
              } else {
                setShowFeedback(true);
              }
            }}
          >
            {showFeedback ? "✗ 提交反馈并拒绝" : "✗ 拒绝并反馈"}
          </button>
        </div>
        <div className="dim small approval-hint">
          「总是允许」会按工具模式写入 <code>.harness9/settings.json</code> 白名单（bash
          按命令首词匹配），可随时在设置中撤销。
        </div>
      </div>
    </div>
  );
}
