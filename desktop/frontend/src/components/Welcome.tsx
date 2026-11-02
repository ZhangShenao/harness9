// Welcome.tsx — 工作目录选择页（首次启动 / 切换项目）。
import { useState } from "react";
import { useStore } from "../store";

export function Welcome() {
  const recentDirs = useStore((s) => s.recentDirs);
  const initialize = useStore((s) => s.initialize);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const pick = async () => {
    setBusy(true);
    setError("");
    try {
      const dir = await window.go.main.App.PickWorkDir();
      if (dir) await initialize(dir);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const use = async (dir: string) => {
    setBusy(true);
    setError("");
    try {
      await initialize(dir);
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  };

  return (
    <div className="center-screen">
      <div className="welcome-card">
        <h1 className="welcome-title">
          <span className="logo-dot big" /> harness9 <span className="dim">Desktop</span>
        </h1>
        <p className="dim">选择一个工作目录开始——Agent 将以它为沙箱根目录读写文件、执行命令。</p>

        <button className="btn btn-primary btn-block btn-lg" disabled={busy} onClick={pick}>
          {busy ? "初始化中…" : "选择工作目录…"}
        </button>

        {recentDirs.length > 0 && (
          <>
            <div className="dim small recent-label">最近使用</div>
            <div className="recent-list">
              {recentDirs.map((d) => (
                <button key={d} className="recent-item" disabled={busy} onClick={() => use(d)}>
                  📁 {d}
                </button>
              ))}
            </div>
          </>
        )}

        {error && <div className="error-text">{error}</div>}
        <div className="dim small tip">
          提示：目录中的 .env 提供 API Key（OPENAI_API_KEY / ORCAROUTER_API_KEY 等），
          AGENTS.md 与 skills/ 会被自动加载。
        </div>
      </div>
    </div>
  );
}
