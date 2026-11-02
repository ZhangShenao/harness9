// Sidebar.tsx — 左栏：会话列表 + 新建任务 + 底部状态（模型/沙箱/设置）。
import { useEffect, useRef, useState } from "react";
import { useStore } from "../store";

function formatTime(unix: number): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  if (sameDay) return hm;
  return `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}

function sandboxBadge(label: string): { text: string; cls: string } {
  switch (label) {
    case "seatbelt":
      return { text: "Seatbelt 沙箱", cls: "badge-green" };
    case "docker":
      return { text: "Docker 沙箱", cls: "badge-green" };
    case "docker-degraded":
      return { text: "沙箱降级", cls: "badge-red" };
    default:
      return { text: "沙箱关闭", cls: "badge-gray" };
  }
}

export function Sidebar() {
  const sessions = useStore((s) => s.sessions);
  const activeId = useStore((s) => s.activeSession?.id);
  const running = useStore((s) => s.running);
  const modelInfo = useStore((s) => s.modelInfo);
  const sandboxLabel = useStore((s) => s.sandboxLabel);
  const degradedReason = useStore((s) => s.degradedReason);
  const workDir = useStore((s) => s.workDir);
  const newSession = useStore((s) => s.newSession);
  const openSession = useStore((s) => s.openSession);
  const deleteSession = useStore((s) => s.deleteSession);
  const renameSession = useStore((s) => s.renameSession);
  const setSettingsOpen = useStore((s) => s.setSettingsOpen);

  const [editingId, setEditingId] = useState<string | null>(null);
  const [editText, setEditText] = useState("");
  const editRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editingId) editRef.current?.focus();
  }, [editingId]);

  const badge = sandboxBadge(sandboxLabel);
  const shortDir = workDir.split("/").filter(Boolean).slice(-2).join("/");

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <div className="logo-row">
          <span className="logo-dot" />
          <span className="logo-text">harness9</span>
        </div>
        <button className="btn btn-primary btn-block" onClick={() => newSession().catch(() => {})}>
          ＋ 新建任务
        </button>
        <div className="dim small dir-row" title={workDir}>
          {shortDir}
        </div>
      </div>

      <div className="session-list">
        {sessions.map((sess) => (
          <div
            key={sess.id}
            className={`session-item ${sess.id === activeId ? "active" : ""}`}
            onClick={() => (running ? undefined : openSession(sess.id).catch(() => {}))}
          >
            {editingId === sess.id ? (
              <input
                ref={editRef}
                className="rename-input"
                value={editText}
                onChange={(e) => setEditText(e.target.value)}
                onBlur={() => {
                  if (editText.trim()) renameSession(sess.id, editText.trim()).catch(() => {});
                  setEditingId(null);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") (e.target as HTMLInputElement).blur();
                  if (e.key === "Escape") setEditingId(null);
                }}
                onClick={(e) => e.stopPropagation()}
              />
            ) : (
              <>
                <div className="session-title">
                  {sess.title || "新会话"}
                  {sess.id === activeId && running && <span className="pulse-dot" />}
                </div>
                <div className="session-meta">
                  <span>{formatTime(sess.updated_at)}</span>
                  <span>{sess.msg_count} 条</span>
                  <button
                    className="icon-btn"
                    title="重命名"
                    onClick={(e) => {
                      e.stopPropagation();
                      setEditingId(sess.id);
                      setEditText(sess.title);
                    }}
                  >
                    ✎
                  </button>
                  <button
                    className="icon-btn"
                    title="删除"
                    onClick={(e) => {
                      e.stopPropagation();
                      deleteSession(sess.id).catch(() => {});
                    }}
                  >
                    🗑
                  </button>
                </div>
              </>
            )}
          </div>
        ))}
        {sessions.length === 0 && <div className="dim small pad8">暂无会话</div>}
      </div>

      <div className="sidebar-foot">
        {degradedReason && <div className="degraded-tip" title={degradedReason}>⚠ {degradedReason}</div>}
        <div className="status-row">
          <span className={`badge ${badge.cls}`}>{badge.text}</span>
          <span className="dim small" title={modelInfo.model}>
            {modelInfo.model}
          </span>
        </div>
        <button className="btn btn-ghost btn-block" onClick={() => setSettingsOpen(true)}>
          ⚙ 设置
        </button>
      </div>
    </aside>
  );
}
