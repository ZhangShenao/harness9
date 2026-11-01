// Composer.tsx — 底部输入区：多行输入、发送/停止、斜杠补全（/skill 与内置命令）。
import { useEffect, useRef, useState } from "react";
import { useStore } from "../store";

interface SlashItem {
  label: string;
  desc: string;
  builtin?: "new" | "compact";
}

export function Composer() {
  const running = useStore((s) => s.running);
  const skills = useStore((s) => s.skills);
  const permissionModes = useStore((s) => s.permissionModes);
  const settings = useStore((s) => s.settings);
  const send = useStore((s) => s.send);
  const interrupt = useStore((s) => s.interrupt);
  const newSession = useStore((s) => s.newSession);
  const compact = useStore((s) => s.compact);
  const saveSettings = useStore((s) => s.saveSettings);

  const [text, setText] = useState("");
  const [slashIdx, setSlashIdx] = useState(-1);
  const taRef = useRef<HTMLTextAreaElement>(null);

  // 斜杠候选：内置命令 + 技能
  const slashQuery = text.startsWith("/") ? text.slice(1).split(" ")[0].toLowerCase() : null;
  const slashItems: SlashItem[] = slashQuery === null ? [] : ([
    { label: "/new", desc: "新建会话", builtin: "new" as const },
    { label: "/compact", desc: "手动压缩上下文", builtin: "compact" as const },
    ...skills.map((sk) => ({ label: `/${sk}`, desc: "加载技能" })),
  ].filter((it) => it.label.toLowerCase().includes(slashQuery)));

  useEffect(() => setSlashIdx(-1), [text]);

  // 自适应高度
  useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, 220) + "px";
  }, [text]);

  const finishSlash = (item: SlashItem) => {
    if (item.builtin) {
      // 内置命令立即执行并清空输入（否则 "/new" 会残留在输入框里）
      setText("");
      if (item.builtin === "new") {
        newSession().catch(() => {});
      } else {
        compact();
      }
    } else {
      setText(item.label + " ");
      taRef.current?.focus();
    }
    setSlashIdx(-1);
  };

  const doSend = async () => {
    const prompt = text.trim();
    if (!prompt || running) return;
    // /skill 展开：技能全文作为 prompt 前缀（对齐 TUI resolvePrompt 语义）
    if (prompt.startsWith("/")) {
      const name = prompt.slice(1).split(" ")[0];
      const rest = prompt.slice(1 + name.length).trim();
      if (skills.includes(name)) {
        const content = await window.go.main.App.GetSkillContent(name);
        if (content) {
          setText("");
          await send(rest ? `${content}\n\n附加要求：${rest}` : content);
          return;
        }
      }
    }
    setText("");
    await send(prompt);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (slashItems.length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIdx((i) => (i + 1) % slashItems.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIdx((i) => (i <= 0 ? slashItems.length - 1 : i - 1));
        return;
      }
      if (e.key === "Enter" && slashIdx >= 0 && !e.metaKey && !e.shiftKey) {
        e.preventDefault();
        finishSlash(slashItems[slashIdx]);
        return;
      }
      if (e.key === "Escape") {
        setSlashIdx(-1);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && (e.metaKey || e.ctrlKey || !e.altKey)) {
      // Enter 直发；Shift+Enter 换行
      if (!e.shiftKey) {
        e.preventDefault();
        doSend().catch(() => {});
      }
    }
    if (e.key === "Escape" && running) {
      interrupt();
    }
  };

  const currentPerm = settings.permission_mode || "default";

  return (
    <div className="composer-wrap">
      {slashItems.length > 0 && (
        <div className="slash-menu">
          {slashItems.map((it, i) => (
            <div
              key={it.label}
              className={`slash-item ${i === slashIdx ? "active" : ""}`}
              onClick={() => finishSlash(it)}
            >
              <span className="slash-label">{it.label}</span>
              <span className="dim small">{it.desc}</span>
            </div>
          ))}
        </div>
      )}
      <div className="composer">
        <textarea
          ref={taRef}
          className="composer-input"
          placeholder={running ? "任务执行中…（Esc 中断）" : "描述任务…（/ 唤起技能与命令）"}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKeyDown}
          disabled={false}
        />
        <div className="composer-row">
          <select
            className="select dim"
            value={currentPerm}
            title="权限模式"
            onChange={(e) => saveSettings(settings.model || "", e.target.value, settings.sandbox_backend || "").catch(() => {})}
          >
            {permissionModes.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
          <div className="spacer" />
          {running ? (
            <button className="btn btn-danger" onClick={() => interrupt()}>
              ■ 停止
            </button>
          ) : (
            <button className="btn btn-primary" disabled={!text.trim()} onClick={() => doSend().catch(() => {})}>
              发送 ⏎
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
