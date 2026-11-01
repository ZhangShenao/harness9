// SettingsDialog.tsx — 设置弹窗：模型 / 权限模式 / 沙箱后端。
// 生效时机在保存后以 hint 提示（权限模式下次打开会话生效；模型与沙箱重启生效）。
import { useState } from "react";
import { useStore } from "../store";

export function SettingsDialog() {
  const settings = useStore((s) => s.settings);
  const permissionModes = useStore((s) => s.permissionModes);
  const saveSettings = useStore((s) => s.saveSettings);
  const modelInfo = useStore((s) => s.modelInfo);
  const workDir = useStore((s) => s.workDir);
  const close = useStore((s) => s.setSettingsOpen);

  const [model, setModel] = useState(settings.model || modelInfo.model || "");
  const [perm, setPerm] = useState(settings.permission_mode || "default");
  const [backend, setBackend] = useState(settings.sandbox_backend || "docker");
  const [hint, setHint] = useState("");
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    try {
      const h = await saveSettings(model.trim(), perm, backend);
      setHint(h || "已保存");
    } catch (e) {
      setHint(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-mask" onClick={() => close(false)}>
      <div className="settings-card" onClick={(e) => e.stopPropagation()}>
        <div className="settings-head">
          <span>设置</span>
          <button className="icon-btn" onClick={() => close(false)}>
            ✕
          </button>
        </div>

        <label className="field">
          <span className="field-label">模型</span>
          <input className="input" value={model} onChange={(e) => setModel(e.target.value)} placeholder="openai/gpt-4o-mini" />
          <span className="dim small">当前上下文窗口：{modelInfo.context_window} tokens。修改后重启应用生效。</span>
        </label>

        <label className="field">
          <span className="field-label">权限模式</span>
          <select className="input" value={perm} onChange={(e) => setPerm(e.target.value)}>
            {permissionModes.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label} — {m.desc}
              </option>
            ))}
          </select>
          <span className="dim small">下次打开会话时生效。</span>
        </label>

        <label className="field">
          <span className="field-label">沙箱后端</span>
          <select className="input" value={backend} onChange={(e) => setBackend(e.target.value)}>
            <option value="seatbelt">Seatbelt（macOS 原生，推荐）</option>
            <option value="docker">Docker（容器级强隔离）</option>
            <option value="off">关闭（本地进程直跑）</option>
          </select>
          <span className="dim small">Seatbelt 为 sandbox-exec 原生沙箱，零依赖秒启；Docker 提供更强隔离但需守护进程。重启应用生效。</span>
        </label>

        <div className="field">
          <span className="field-label">工作目录</span>
          <div className="dim small mono">{workDir}</div>
        </div>

        {hint && <div className="save-hint">{hint}</div>}

        <div className="settings-actions">
          <button className="btn btn-ghost" onClick={() => close(false)}>
            关闭
          </button>
          <button className="btn btn-primary" disabled={busy} onClick={save}>
            {busy ? "保存中…" : "保存"}
          </button>
        </div>
      </div>
    </div>
  );
}
