// RightPanel.tsx — 右侧审查面板：Plan 进度 / Diff 审查 两个标签页。
// diff 按 "diff --git" 切分为每文件折叠块，+/- 行着色。
import { useEffect, useMemo, useState } from "react";
import { useStore } from "../store";

const statusIcon: Record<string, string> = {
  pending: "○",
  in_progress: "◐",
  completed: "●",
  cancelled: "⊘",
};

function PlanTab() {
  const plan = useStore((s) => s.plan);
  if (plan.length === 0) {
    return <div className="dim small pad8">尚无计划。Agent 处理复杂任务时会通过 plan_write 生成执行计划。</div>;
  }
  const done = plan.filter((p) => p.status === "completed").length;
  return (
    <div className="plan-list">
      <div className="plan-progress">
        <div className="plan-progress-track">
          <div className="plan-progress-fill" style={{ width: `${(done / plan.length) * 100}%` }} />
        </div>
        <span className="dim small">
          {done}/{plan.length} 完成
        </span>
      </div>
      {plan.map((p) => (
        <div key={p.id} className={`plan-item status-${p.status}`}>
          <span className="plan-icon">{statusIcon[p.status] ?? "○"}</span>
          <span className={p.status === "completed" ? "strike dim" : ""}>{p.content}</span>
        </div>
      ))}
    </div>
  );
}

function DiffTab() {
  const diff = useStore((s) => s.diff);
  const refresh = useStore((s) => s.refreshDiff);
  const [openFiles, setOpenFiles] = useState<Set<string>>(new Set());

  const files = useMemo(() => {
    if (!diff?.patch) return [];
    return diff.patch.split("diff --git ").filter((x) => x.trim()).map((chunk) => {
      const firstLineEnd = chunk.indexOf("\n");
      const header = chunk.slice(0, firstLineEnd);
      const m = header.match(/ b\/(.+)$/);
      return { name: m ? m[1] : header, body: "diff --git " + chunk };
    });
  }, [diff]);

  if (!diff?.is_git_repo) {
    return <div className="dim small pad8">当前工作目录不是 git 仓库，无法生成变更审查。</div>;
  }

  const toggle = (name: string) => {
    setOpenFiles((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  return (
    <div className="diff-list">
      <div className="diff-head">
        <span className="badge badge-blue">{diff.branch || "detached"}</span>
        <button className="icon-btn" title="刷新" onClick={() => refresh().catch(() => {})}>
          ⟳
        </button>
      </div>
      {!diff.dirty && <div className="dim small pad8">工作区干净，没有未提交变更。</div>}
      {diff.stat && <pre className="diff-stat">{diff.stat}</pre>}
      {diff.untracked && diff.untracked.length > 0 && (
        <div className="untracked">
          <div className="dim small">未跟踪文件（{diff.untracked.length}）</div>
          {diff.untracked.map((f) => (
            <div key={f} className="untracked-file">
              ＋ {f}
            </div>
          ))}
        </div>
      )}
      {files.map((f) => (
        <div key={f.name} className="diff-file">
          <div className="diff-file-head" onClick={() => toggle(f.name)}>
            <span>{openFiles.has(f.name) ? "▾" : "▸"}</span>
            <span className="diff-file-name">{f.name}</span>
          </div>
          {openFiles.has(f.name) && (
            <pre className="diff-body">
              {f.body.split("\n").map((line, i) => (
                <div
                  key={i}
                  className={
                    line.startsWith("+") && !line.startsWith("+++")
                      ? "line-add"
                      : line.startsWith("-") && !line.startsWith("---")
                        ? "line-del"
                        : line.startsWith("@@")
                          ? "line-hunk"
                          : ""
                  }
                >
                  {line}
                </div>
              ))}
            </pre>
          )}
        </div>
      ))}
    </div>
  );
}

export function RightPanel() {
  const tab = useStore((s) => s.rightTab);
  const setTab = useStore((s) => s.setRightTab);
  const plan = useStore((s) => s.plan);
  const diff = useStore((s) => s.diff);
  const hasContent = plan.length > 0 || (diff?.dirty ?? false);

  // 首个计划出现时自动展开
  useEffect(() => {
    if (plan.length > 0 && tab === null) setTab("plan");
  }, [plan, tab, setTab]);

  if (!tab) {
    return (
      <div className="right-rail">
        {hasContent && (
          <button className="rail-btn" title="计划" onClick={() => setTab("plan")}>
            ☑
          </button>
        )}
        <button className="rail-btn" title="Diff 审查" onClick={() => setTab("diff")}>
          ±
        </button>
      </div>
    );
  }

  return (
    <aside className="right-panel">
      <div className="right-tabs">
        <button className={`right-tab ${tab === "plan" ? "active" : ""}`} onClick={() => setTab("plan")}>
          计划
        </button>
        <button className={`right-tab ${tab === "diff" ? "active" : ""}`} onClick={() => setTab("diff")}>
          Diff 审查
        </button>
        <button className="icon-btn right-close" onClick={() => setTab(null)}>
          ✕
        </button>
      </div>
      <div className="right-body">{tab === "plan" ? <PlanTab /> : <DiffTab />}</div>
    </aside>
  );
}
