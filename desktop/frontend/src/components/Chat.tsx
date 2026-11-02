// Chat.tsx — 中央对话流：时间线渲染（用户/正文/Thinking/工具卡片/通知）
// + 流式中间态 + 底部 token 计量条。
import { useEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { formatTokens, tokenState, useStore } from "../store";
import type { ChatItem } from "../types";

function ThinkingBlock({ text, streaming }: { text: string; streaming?: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <div className={`thinking-block ${streaming ? "streaming" : ""}`} onClick={() => setOpen(!open)}>
      <div className="thinking-head">
        {streaming ? <span className="pulse-dot" /> : "▸"} 思考过程{streaming ? "（流式中）" : ""}
      </div>
      {(open || streaming) && <pre className="thinking-body">{text}</pre>}
    </div>
  );
}

function ToolCard({ item }: { item: ChatItem }) {
  const [open, setOpen] = useState(false);
  const hasBody = !!item.toolOutput || !!item.toolArgs;
  const status = item.running ? "running" : item.isError ? "error" : "ok";
  return (
    <div className={`tool-card status-${status}`}>
      <div className="tool-head" onClick={() => hasBody && setOpen(!open)}>
        <span className="tool-status">
          {item.running ? <span className="pulse-dot" /> : item.isError ? "✗" : "✓"}
        </span>
        <span className="tool-name">{item.toolName}</span>
        <span className="tool-args-dim">{argPreview(item.toolArgs)}</span>
        {!!item.durationMs && <span className="tool-dur">{item.durationMs}ms</span>}
        {hasBody && <span className="tool-chevron">{open ? "▾" : "▸"}</span>}
      </div>
      {open && (
        <div className="tool-body">
          {item.toolArgs && (
            <>
              <div className="dim small">参数</div>
              <pre>{prettyJSON(item.toolArgs) ?? item.toolArgs}</pre>
            </>
          )}
          {item.toolOutput && (
            <>
              <div className="dim small">输出</div>
              <pre>{item.toolOutput}</pre>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function argPreview(args?: string): string {
  if (!args) return "";
  if (typeof args !== "string") {
    // 双保险：store 已归一化，此处兜底非字符串输入
    try {
      return argPreview(JSON.stringify(args));
    } catch {
      return "";
    }
  }
  try {
    const obj = JSON.parse(args);
    const first = obj.command ?? obj.path ?? obj.file_path ?? obj.query ?? obj.skill_name ?? obj.action ?? "";
    if (first) return String(first).slice(0, 80);
    return "";
  } catch {
    return args.slice(0, 60);
  }
}

function prettyJSON(s?: string): string | null {
  if (!s) return null;
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return null;
  }
}

function SubAgentCard({ item }: { item: ChatItem }) {
  const [open, setOpen] = useState(false);
  const tail = (item.lines ?? []).slice(-3);
  return (
    <div className="subagent-card" onClick={() => setOpen(!open)}>
      <div className="subagent-head">
        🤖 子代理 {item.agentName} <span className="dim small">（{item.lines?.length ?? 0} 条进度，点击展开）</span>
      </div>
      {(open ? item.lines ?? [] : tail).map((l, i) => (
        <div key={i} className="subagent-line">
          {l}
        </div>
      ))}
    </div>
  );
}

function TokenBar() {
  const modelInfo = useStore((s) => s.modelInfo);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 500);
    return () => clearInterval(id);
  }, []);
  const used = tokenState.estimated;
  const window_ = tokenState.window || modelInfo.context_window || 1;
  const pct = Math.min(100, Math.round((used / window_) * 100));
  const cls = pct > 80 ? "red" : pct > 50 ? "yellow" : "green";
  return (
    <div className="token-bar">
      <div className="token-track">
        <div className={`token-fill ${cls}`} style={{ width: `${Math.max(pct, 1)}%` }} />
      </div>
      <span className="dim small" key={tick}>
        {formatTokens(used)} / {formatTokens(window_)} tokens（{pct}%）
      </span>
    </div>
  );
}

export function Chat() {
  const items = useStore((s) => s.items);
  const streamingAssistant = useStore((s) => s.streamingAssistant);
  const streamingThinking = useStore((s) => s.streamingThinking);
  const streamingTools = useStore((s) => s.streamingTools);
  const plan = useStore((s) => s.plan);
  const activeSession = useStore((s) => s.activeSession);
  const scrollRef = useRef<HTMLDivElement>(null);

  // 流式期间自动滚底（用户手动上滚则不打扰）
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 160;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }, [items, streamingAssistant, streamingThinking, streamingTools]);

  const inProgress = streamingAssistant || streamingThinking || streamingTools.size > 0;
  const planActive = plan.filter((p) => p.status === "pending" || p.status === "in_progress").length;

  return (
    <div className="chat-col">
      <div className="chat-head">
        <span className="chat-title">{activeSession?.title || "新会话"}</span>
        {planActive > 0 && (
          <span className="badge badge-blue">
            计划 {plan.filter((p) => p.status === "completed").length}/{plan.length}
          </span>
        )}
      </div>
      <div className="chat-scroll" ref={scrollRef}>
        {items.length === 0 && !inProgress && (
          <div className="chat-empty">
            <div className="welcome-title small">
              <span className="logo-dot" /> harness9
            </div>
            <div className="dim">描述一个任务开始。复杂任务 Agent 会先用 plan_write 规划再执行。</div>
          </div>
        )}
        {items.map((item) => (
          <TimelineRow key={item.key} item={item} />
        ))}
        {streamingThinking && <ThinkingBlock text={streamingThinking} streaming />}
        {[...streamingTools.values()].map((card) => (
          <ToolCard key={card.key} item={card} />
        ))}
        {streamingAssistant && (
          <div className="msg assistant">
            <Markdown remarkPlugins={[remarkGfm]}>{streamingAssistant}</Markdown>
            <span className="cursor">▍</span>
          </div>
        )}
        {!inProgress && items.length > 0 && <div className="dim small end-mark">— 本轮结束 —</div>}
      </div>
      <TokenBar />
    </div>
  );
}

function TimelineRow({ item }: { item: ChatItem }) {
  switch (item.kind) {
    case "user":
      return (
        <div className="msg user">
          <div className="user-bubble">{item.text}</div>
        </div>
      );
    case "assistant":
      return (
        <div className="msg assistant">
          <Markdown remarkPlugins={[remarkGfm]}>{item.text ?? ""}</Markdown>
        </div>
      );
    case "thinking":
      return <ThinkingBlock text={item.text ?? ""} />;
    case "tool":
      return <ToolCard item={item} />;
    case "subagent":
      return <SubAgentCard item={item} />;
    case "notice":
      return (
        <div className="msg notice">
          <span className="notice-title">{item.title}</span>
          {item.text && <span className="dim"> — {item.text}</span>}
        </div>
      );
    case "error":
      return (
        <div className="msg error-line">
          ❌ {item.text}
        </div>
      );
    default:
      return null;
  }
}
