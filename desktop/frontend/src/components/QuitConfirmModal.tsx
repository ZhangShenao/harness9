// QuitConfirmModal.tsx — 退出确认弹窗（替代原生 MessageDialog：
// 原生对话框在 beforeClose 的主线程上下文中存在点击无响应的兼容性问题）。
// 触发链路：关窗/Cmd+Q → Go beforeClose → app:quit-requested 事件 → 本弹窗；
// 「退出」→ ConfirmQuit 绑定（置旁路位 + Quit），「取消」→ 仅关弹窗。
import { useStore } from "../store";

export function QuitConfirmModal() {
  const open = useStore((s) => s.quitConfirmOpen);
  const setOpen = useStore((s) => s.setQuitConfirmOpen);
  const confirmQuit = useStore((s) => s.confirmQuit);

  if (!open) return null;

  return (
    <div className="modal-mask">
      <div className="approval-card">
        <div className="approval-head">
          <span className="approval-icon">🚪</span>
          <div>
            <div className="approval-title">退出 harness9？</div>
            <div className="dim small">Agent 任务尚未结束，退出将中断当前任务（未完成的轨迹不会持久化）。</div>
          </div>
        </div>
        <div className="approval-actions">
          <button className="btn btn-danger" onClick={() => confirmQuit().catch(() => {})}>
            退出
          </button>
          <button className="btn btn-ghost" onClick={() => setOpen(false)}>
            继续运行
          </button>
        </div>
      </div>
    </div>
  );
}
