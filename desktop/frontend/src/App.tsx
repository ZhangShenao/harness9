// App.tsx — 布局骨架与初始化流程：
// loading（读最近目录）→ welcome（选工作目录）→ ready（三栏工作台）。
// agent:event 事件统一在此订阅并转发给 store。
import { useEffect, useState } from "react";
import { useStore } from "./store";
import { Sidebar } from "./components/Sidebar";
import { Chat } from "./components/Chat";
import { Composer } from "./components/Composer";
import { ApprovalModal } from "./components/ApprovalModal";
import { RightPanel } from "./components/RightPanel";
import { SettingsDialog } from "./components/SettingsDialog";
import { Welcome } from "./components/Welcome";
import type { AgentEvent } from "./types";

export default function App() {
  const phase = useStore((s) => s.phase);
  const settingsOpen = useStore((s) => s.settingsOpen);
  const errorMsg = useStore((s) => s.errorMsg);
  const clearError = useStore((s) => s.clearError);
  const handleEvent = useStore((s) => s.handleEvent);
  const setWelcome = useStore((s) => s.setWelcome);
  const hasApprovals = useStore((s) => s.approvals.length > 0);
  const [bootstrapped, setBootstrapped] = useState(false);

  useEffect(() => {
    if (bootstrapped) return;
    setBootstrapped(true);
    // 订阅引擎事件流
    window.runtime.EventsOn("agent:event", (evt: AgentEvent) => handleEvent(evt));
    // 冷启动：有最近目录时仍先进入欢迎页（用户可一键续用），
    // 保证多项目场景下 workDir 是显式决策。
    window.go.main.App.RecentWorkDirs().then(
      (dirs) => setWelcome(dirs ?? []),
      () => setWelcome([])
    );
  }, [bootstrapped, handleEvent, setWelcome]);

  if (phase === "loading") {
    return (
      <div className="center-screen">
        <div className="spinner" />
        <div className="dim">正在启动 harness9…</div>
      </div>
    );
  }

  if (phase === "welcome") {
    return <Welcome />;
  }

  return (
    <div className="app-layout">
      <Sidebar />
      <main className="main-col">
        <Chat />
        <Composer />
      </main>
      <RightPanel />
      {hasApprovals && <ApprovalModal />}
      {settingsOpen && <SettingsDialog />}
      {errorMsg && (
        <div className="toast toast-error" onClick={clearError}>
          {errorMsg}（点击关闭）
        </div>
      )}
    </div>
  );
}
