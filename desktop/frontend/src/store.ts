// store.ts — zustand 全局状态：会话、时间线、流式缓冲、审批队列。
// 事件装配语义与 TUI handleEvent 对齐：流式 delta 进缓冲、工具卡片
// 按事件插值、done/error 收尾。与 TUI 的差异：审批不阻塞事件消费
//（appservice 泵已解耦），多审批排队展示。
import { create } from "zustand";
import type {
  AgentEvent,
  ApprovalData,
  ChatItem,
  DiffResult,
  InitResult,
  ModelInfo,
  PermissionModeOption,
  PlanItem,
  SessionInfo,
  Settings,
  SubAgentPayload,
  TokenUpdatePayload,
  ToolCallPayload,
  ToolResultPayload,
} from "./types";

let itemSeq = 0;
const nextKey = (prefix: string) => `${prefix}_${++itemSeq}`;

export interface UiState {
  phase: "loading" | "welcome" | "ready";
  version: string;
  workDir: string;
  recentDirs: string[];

  sessions: SessionInfo[];
  activeSession: SessionInfo | null;
  items: ChatItem[];

  running: boolean;
  modelInfo: ModelInfo;
  sandboxLabel: string;
  sandboxStatus: Array<Partial<SandboxInfoRow>>;
  degradedReason: string;
  skills: string[];
  settings: Settings;
  permissionModes: PermissionModeOption[];

  plan: PlanItem[];
  approvals: ApprovalData[];
  diff: DiffResult | null;

  // 流式中间态
  streamingAssistant: string;
  streamingThinking: string;
  streamingTools: Map<string, ChatItem>;

  // UI 面板
  rightTab: "plan" | "diff" | null;
  settingsOpen: boolean;
  errorMsg: string;
  quitConfirmOpen: boolean;

  // actions
  initFromResult: (r: InitResult) => void;
  setWelcome: (recent: string[]) => void;
  handleEvent: (evt: AgentEvent) => void;
  send: (prompt: string) => Promise<void>;
  interrupt: () => Promise<void>;
  newSession: () => Promise<void>;
  openSession: (id: string) => Promise<void>;
  deleteSession: (id: string) => Promise<void>;
  renameSession: (id: string, title: string) => Promise<void>;
  respondApproval: (id: string, approved: boolean, feedback: string, remember: boolean) => Promise<void>;
  compact: () => Promise<void>;
  refreshPlan: () => Promise<void>;
  refreshDiff: () => Promise<void>;
  refreshSandbox: () => Promise<void>;
  saveSettings: (model: string, permissionMode: string, sandboxBackend: string) => Promise<string>;
  initialize: (workDir: string) => Promise<void>;
  setRightTab: (tab: "plan" | "diff" | null) => void;
  setSettingsOpen: (open: boolean) => void;
  setQuitConfirmOpen: (open: boolean) => void;
  confirmQuit: () => Promise<void>;
  clearError: () => void;
}

// SandboxInfoRow 规避对 Go 序列化字段大小写的强依赖。
export type SandboxInfoRow = {
  ID?: string;
  DockerID?: string;
  State?: string;
  WorkDir?: string;
  Image?: string;
};

const App = () => window.go.main.App;

export const useStore = create<UiState>((set, get) => ({
  phase: "loading",
  version: "",
  workDir: "",
  recentDirs: [],

  sessions: [],
  activeSession: null,
  items: [],

  running: false,
  modelInfo: { model: "", context_window: 0 },
  sandboxLabel: "",
  sandboxStatus: [],
  degradedReason: "",
  skills: [],
  settings: {},
  permissionModes: [],

  plan: [],
  approvals: [],
  diff: null,

  streamingAssistant: "",
  streamingThinking: "",
  streamingTools: new Map(),

  rightTab: null,
  settingsOpen: false,
  errorMsg: "",
  quitConfirmOpen: false,

  // ---- 初始化 ----

  setWelcome: (recent) => set({ phase: "welcome", recentDirs: recent }),

  initFromResult: (r) => {
    set({
      phase: "ready",
      version: r.version,
      workDir: r.work_dir,
      sessions: r.sessions,
      activeSession: r.active_session,
      items: r.items.map(historyToChatItem),
      plan: r.plan ?? [],
      modelInfo: r.model_info,
      sandboxLabel: r.sandbox_label,
      sandboxStatus: r.sandbox_status ?? [],
      degradedReason: r.degraded_reason ?? "",
      skills: r.skills ?? [],
      settings: r.settings,
      permissionModes: r.permission_modes,
      running: false,
      streamingAssistant: "",
      streamingThinking: "",
      streamingTools: new Map(),
      approvals: [],
      rightTab: null,
    });
  },

  initialize: async (workDir) => {
    const r = await App().Initialize(workDir);
    get().initFromResult(r);
    get().refreshDiff().catch(() => {});
  },

  // ---- 事件装配 ----

  handleEvent: (evt) => {
    const s = get();
    switch (evt.type) {
      case "action_delta":
        set({ streamingAssistant: s.streamingAssistant + String(evt.data ?? "") });
        break;
      case "thinking_delta":
        set({ streamingThinking: s.streamingThinking + String(evt.data ?? "") });
        break;
      case "tool_start": {
        const tc = evt.data as ToolCallPayload;
        // 工具边界：先固化已缓冲的 thinking/正文，再以【全新】的 streaming map
        // 承载新卡片——沿用 flush 前的旧 map 会把上一张刚固化进 items 的卡片
        // 再复制一份（两处各出现一张，且后续 tool_result 只更新流式副本）。
        flushStreaming(set);
        const tools = new Map<string, ChatItem>();
        tools.set(tc.id, {
          key: `tool_${tc.id}`,
          kind: "tool",
          toolCallId: tc.id,
          toolName: tc.name,
          toolArgs: normalizeToolArgs(tc.arguments),
          running: true,
        });
        set({ streamingTools: tools });
        break;
      }
      case "tool_result": {
        const tr = evt.data as ToolResultPayload;
        const tools = new Map(s.streamingTools);
        const card = tools.get(tr.Result.tool_call_id);
        if (card) {
          tools.set(tr.Result.tool_call_id, {
            ...card,
            toolOutput: tr.Result.output,
            isError: tr.Result.is_error,
            running: false,
            durationMs: nsToMs(tr.Duration),
          });
        } else {
          // 无 Start 的结果（恢复场景）：补一张卡
          tools.set(tr.Result.tool_call_id, {
            key: `tool_${tr.Result.tool_call_id}`,
            kind: "tool",
            toolCallId: tr.Result.tool_call_id,
            toolName: "(unknown)",
            toolOutput: tr.Result.output,
            isError: tr.Result.is_error,
            running: false,
            durationMs: nsToMs(tr.Duration),
          });
        }
        set({ streamingTools: tools });
        break;
      }
      case "token_update": {
        const tp = evt.data as TokenUpdatePayload;
        tokenState.estimated = tp.estimated_tokens;
        tokenState.window = tp.context_window || s.modelInfo.context_window;
        break;
      }
      case "compaction": {
        const rec = evt.data as Record<string, number | string>;
        flushStreaming(set);
        set((st) => ({
          items: [
            ...st.items,
            {
              key: nextKey("notice"),
              kind: "notice",
              title: "上下文已压缩",
              text: `tokens ${rec?.tokens_before ?? "?"} → ${rec?.tokens_after ?? "?"}`,
            },
          ],
        }));
        break;
      }
      case "sub_agent": {
        const sp = evt.data as SubAgentPayload;
        set((st) => {
          const items = [...st.items];
          const last = items[items.length - 1];
          const line = subAgentLine(sp);
          if (last && last.kind === "subagent" && last.agentName === sp.agent_name) {
            items[items.length - 1] = { ...last, lines: [...(last.lines ?? []), line] };
          } else {
            items.push({ key: nextKey("subagent"), kind: "subagent", agentName: sp.agent_name, lines: [line] });
          }
          return { items };
        });
        break;
      }
      case "approval_required": {
        const ad = evt.data as ApprovalData;
        set({ approvals: [...s.approvals, ad] });
        break;
      }
      case "error":
        flushStreaming(set);
        set((st) => ({
          running: false,
          approvals: [], // run 已结束，未应答审批全部失效（服务端同样会清理）
          items: [...st.items, { key: nextKey("error"), kind: "error", text: String(evt.data ?? "未知错误") }],
        }));
        break;
      case "done":
        flushStreaming(set);
        set({ running: false, approvals: [] });
        // 任务结束后刷新计划与 diff（对标 Codex 的 review 闭环）
        get().refreshPlan().catch(() => {});
        get().refreshDiff().catch(() => {});
        break;
    }
  },

  // ---- 动作 ----

  send: async (prompt) => {
    const s = get();
    if (s.running || !prompt.trim()) return;
    set((st) => ({
      running: true,
      streamingAssistant: "",
      streamingThinking: "",
      streamingTools: new Map(),
      items: [...st.items, { key: nextKey("user"), kind: "user", text: prompt }],
    }));
    try {
      await App().SendMessage(prompt);
    } catch (e) {
      set((st) => ({
        running: false,
        items: [...st.items, { key: nextKey("error"), kind: "error", text: String(e) }],
      }));
    }
  },

  interrupt: async () => {
    App().Interrupt();
  },

  newSession: async () => {
    const [sessions, active] = await App().NewSession();
    set({
      sessions,
      activeSession: active,
      items: [],
      plan: [],
      running: false,
      streamingAssistant: "",
      streamingThinking: "",
      streamingTools: new Map(),
      approvals: [],
      rightTab: null,
    });
    get().refreshDiff().catch(() => {});
  },

  openSession: async (id) => {
    if (get().running) return;
    const [info, items, plan] = await App().OpenSession(id);
    set({
      activeSession: info,
      items: (items ?? []).map(historyToChatItem),
      plan: plan ?? [],
      streamingAssistant: "",
      streamingThinking: "",
      streamingTools: new Map(),
      approvals: [],
      rightTab: null,
    });
    get().refreshDiff().catch(() => {});
  },

  deleteSession: async (id) => {
    const sessions = await App().DeleteSession(id);
    const s = get();
    if (s.activeSession?.id === id) {
      const next = sessions[0];
      if (next) {
        await get().openSession(next.id);
      } else {
        set({ activeSession: null, items: [], plan: [] });
      }
    }
    set({ sessions });
  },

  renameSession: async (id, title) => {
    await App().RenameSession(id, title);
    set((st) => ({
      sessions: st.sessions.map((x) => (x.id === id ? { ...x, title } : x)),
      activeSession:
        st.activeSession?.id === id ? { ...st.activeSession, title } : st.activeSession,
    }));
  },

  respondApproval: async (id, approved, feedback, remember) => {
    set((st) => ({ approvals: st.approvals.filter((a) => a.id !== id) }));
    try {
      await App().RespondApproval(id, approved, feedback, remember);
    } catch (e) {
      set({ errorMsg: String(e) });
    }
  },

  compact: async () => {
    try {
      const rec = await App().Compact();
      const before = (rec as any)?.tokens_before ?? (rec as any)?.TokensBefore ?? "?";
      const after = (rec as any)?.tokens_after ?? (rec as any)?.TokensAfter ?? "?";
      set((st) => ({
        items: [
          ...st.items,
          { key: nextKey("notice"), kind: "notice", title: "已手动压缩上下文", text: `tokens ${before} → ${after}` },
        ],
      }));
    } catch (e) {
      set({ errorMsg: String(e) });
    }
  },

  refreshPlan: async () => {
    try {
      set({ plan: (await App().GetCurrentPlan()) ?? [] });
    } catch {
      /* 忽略：会话切换竞态 */
    }
  },

  refreshDiff: async () => {
    try {
      set({ diff: await App().GetSessionDiff() });
    } catch {
      set({ diff: null });
    }
  },

  refreshSandbox: async () => {
    try {
      set({ sandboxStatus: (await App().GetSandboxStatus()) ?? [] });
    } catch {
      /* 忽略 */
    }
  },

  saveSettings: async (model, permissionMode, sandboxBackend) => {
    const hint = await App().SaveSettings(model, permissionMode, sandboxBackend);
    set((st) => ({ settings: { ...st.settings, model, permission_mode: permissionMode, sandbox_backend: sandboxBackend } }));
    return hint ?? "";
  },

  setRightTab: (tab) => set({ rightTab: tab }),
  setSettingsOpen: (open) => set({ settingsOpen: open }),
  setQuitConfirmOpen: (open) => set({ quitConfirmOpen: open }),
  confirmQuit: async () => {
    await window.go.main.App.ConfirmQuit();
  },
  clearError: () => set({ errorMsg: "" }),
}));

// tokenState 是高频更新的 token 计数（不走 zustand，避免每 token 重渲染整树）。
export const tokenState = { estimated: 0, window: 0 };

// flushStreaming 把流式缓冲固化为时间线条目。
function flushStreaming(set: (fn: (st: UiState) => Partial<UiState>) => void) {
  set((st) => {
    const additions: ChatItem[] = [];
    if (st.streamingThinking) {
      additions.push({ key: nextKey("thinking"), kind: "thinking", text: st.streamingThinking });
    }
    if (st.streamingAssistant) {
      additions.push({ key: nextKey("assistant"), kind: "assistant", text: st.streamingAssistant });
    }
    for (const card of st.streamingTools.values()) {
      additions.push(card);
    }
    if (additions.length === 0) return {};
    return {
      items: [...st.items, ...additions],
      streamingThinking: "",
      streamingAssistant: "",
      streamingTools: new Map(),
    };
  });
}

// historyToChatItem 把后端历史条目映射为 UI 时间线（工具卡片含结果回填）。
function historyToChatItem(h: import("./types").HistoryItem): ChatItem {
  if (h.kind === "tool") {
    return {
      key: `tool_${h.tool_call_id}`,
      kind: "tool",
      toolCallId: h.tool_call_id,
      toolName: h.tool_name,
      toolArgs: h.tool_args,
      toolOutput: h.tool_output,
      isError: h.is_error,
      running: h.pending,
    };
  }
  return { key: nextKey(h.kind), kind: h.kind as ChatItem["kind"], text: h.text };
}

// subAgentLine 把子代理进度事件渲染为一行摘要。
function subAgentLine(sp: SubAgentPayload): string {
  switch (sp.kind) {
    case "start":
      return `▶ ${sp.agent_name} 启动`;
    case "delta":
      return sp.text;
    case "tool_start":
      return `🔧 ${sp.tool_name}`;
    case "tool_result":
      return sp.is_error ? `✗ ${sp.tool_name} 失败` : `✓ ${sp.tool_name}`;
    case "done":
      return `■ ${sp.agent_name} 完成`;
    case "error":
      return `✗ ${sp.text}`;
    default:
      return sp.text || sp.kind;
  }
}

// normalizeToolArgs 把 json.RawMessage 经事件桥到达的参数统一为 JSON 文本。
// 事件路径是对象、历史路径是字符串；不归一化会让 argPreview 的 JSON.parse
// 与 args.slice 对对象抛 TypeError，直接白屏。
function normalizeToolArgs(args: unknown): string {
  if (args == null) return "";
  if (typeof args === "string") return args;
  try {
    return JSON.stringify(args);
  } catch {
    return String(args);
  }
}

// nsToMs 把 Go time.Duration（纳秒）转为毫秒展示值。
function nsToMs(ns: number): number {
  if (!ns || ns <= 0) return 0;
  return Math.round(ns / 1e6);
}

// formatTokens 千分位缩写（12.3k / 1.2M），与 TUI FormatTokenCount 语义一致。
export function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}
