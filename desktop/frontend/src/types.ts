// types.ts — 与 Go 侧 DTO 一一对应的类型契约。
// 命名沿用后端 JSON tag（snake_case），禁止在此处改名。

// ---- Wails 运行时全局对象声明 ----
// 桌面端直接使用 Wails 注入的 window.go / window.runtime，
// 避免 wailsjs 生成代码的路径耦合。
export interface WailsWindow {
  go: {
    main: {
      App: {
        Initialize(workDir: string): Promise<InitResult>;
        PickWorkDir(): Promise<string>;
        RecentWorkDirs(): Promise<string[]>;
        ListSessions(): Promise<SessionInfo[]>;
        NewSession(): Promise<[SessionInfo[], SessionInfo]>;
        OpenSession(id: string): Promise<[SessionInfo, HistoryItem[], PlanItem[]]>;
        DeleteSession(id: string): Promise<SessionInfo[]>;
        RenameSession(id: string, title: string): Promise<void>;
        SendMessage(prompt: string): Promise<void>;
        Interrupt(): Promise<void>;
        IsRunning(): Promise<boolean>;
        Compact(): Promise<CompactionRecord>;
        RespondApproval(id: string, approved: boolean, feedback: string, remember: boolean): Promise<void>;
        PendingApprovals(): Promise<ApprovalData[]>;
        GetCurrentPlan(): Promise<PlanItem[]>;
        GetSessionDiff(): Promise<DiffResult>;
        GetModelInfo(): Promise<ModelInfo>;
        GetSandboxStatus(): Promise<SandboxInfo[]>;
        GetSkills(): Promise<string[]>;
        GetSkillContent(name: string): Promise<string>;
        SaveSettings(model: string, permissionMode: string, sandboxBackend: string): Promise<string>;
        ConfirmQuit(): Promise<void>;
        CancelQuit(): Promise<void>;
      };
    };
  };
  runtime: {
    EventsOn(name: string, callback: (...data: any) => void): void;
    EventsOff(name: string, ...additionalKeys: string[]): void;
  };
}

declare global {
  interface Window {
    go: WailsWindow["go"];
    runtime: WailsWindow["runtime"];
  }
}

// ---- DTO ----

export interface SessionInfo {
  id: string;
  title: string;
  created_at: number;
  updated_at: number;
  msg_count: number;
}

export interface HistoryItem {
  kind: "user" | "assistant" | "tool";
  text?: string;
  tool_call_id?: string;
  tool_name?: string;
  tool_args?: string;
  tool_output?: string;
  is_error?: boolean;
  pending?: boolean;
}

export interface PlanItem {
  id: string;
  content: string;
  status: "pending" | "in_progress" | "completed" | "cancelled";
}

export interface ModelInfo {
  model: string;
  context_window: number;
}

export interface SandboxInfo {
  ID: string;
  DockerID: string;
  State: string; // Pending | Running | Stopping | Terminated | Failed
  WorkDir: string;
  Image: string;
}

export interface ApprovalData {
  id: string;
  tool_name: string;
  args: string;
  reason: string;
  risk_level: string; // low | medium | high
}

export interface DiffResult {
  is_git_repo: boolean;
  branch?: string;
  stat?: string;
  patch?: string;
  untracked?: string[];
  dirty: boolean;
}

export interface CompactionRecord {
  tier?: string;
  tokens_before?: number;
  tokens_after?: number;
  msgs_before?: number;
  msgs_after?: number;
  [key: string]: unknown;
}

export interface PermissionModeOption {
  value: string;
  label: string;
  desc: string;
}

export interface Settings {
  model?: string;
  permission_mode?: string;
  sandbox_backend?: string;
  recent_workdirs?: string[];
}

export interface InitResult {
  work_dir: string;
  version: string;
  active_session: SessionInfo;
  sessions: SessionInfo[];
  items: HistoryItem[];
  plan: PlanItem[];
  model_info: ModelInfo;
  sandbox_label: string;
  sandbox_status: SandboxInfo[];
  degraded_reason?: string;
  skills: string[];
  settings: Settings;
  permission_modes: PermissionModeOption[];
}

// ---- agent:event 事件契约（appservice.Event）----

export interface AgentEvent {
  type:
    | "action_delta"
    | "thinking_delta"
    | "tool_start"
    | "tool_result"
    | "done"
    | "error"
    | "token_update"
    | "compaction"
    | "approval_required"
    | "sub_agent";
  turn: number;
  data: unknown;
}

export interface ToolCallPayload {
  id: string;
  name: string;
  // schema.ToolCall.Arguments 是 json.RawMessage：经事件桥 JSON 序列化后
  // 到前端是【对象】（历史记录路径 HistoryItem.tool_args 才是字符串）。
  // 消费前必须经 normalizeToolArgs 归一化为文本，否则 JSON.parse/
  // args.slice 会对对象抛 TypeError 直接白屏（前端无错误边界）。
  arguments: string | Record<string, unknown> | null;
}

// engine.ToolResultData：无 json tag，字段名保持 Go 大写导出名。
export interface ToolResultPayload {
  Result: { tool_call_id: string; output: string; is_error: boolean };
  Duration: number; // Go time.Duration 序列化后的纳秒
}

// engine.TokenUpdateData：snake_case json tag。
export interface TokenUpdatePayload {
  estimated_tokens: number;
  context_window: number;
}

// schema.SubAgentUpdate：snake_case json tag。
export interface SubAgentPayload {
  agent_name: string;
  kind: string;
  text: string;
  tool_name: string;
  is_error: boolean;
}

// ---- UI 时间线条目（前端组装视图用，含流式中间态）----

export type ChatItemKind =
  | "user"
  | "assistant"
  | "thinking"
  | "tool"
  | "notice"
  | "error"
  | "subagent";

export interface ChatItem {
  key: string;
  kind: ChatItemKind;
  text?: string;
  streaming?: boolean;
  // tool 卡片
  toolCallId?: string;
  toolName?: string;
  toolArgs?: string;
  toolOutput?: string;
  isError?: boolean;
  running?: boolean;
  durationMs?: number;
  // notice / error
  title?: string;
  // subagent 聚合
  agentName?: string;
  lines?: string[];
}
