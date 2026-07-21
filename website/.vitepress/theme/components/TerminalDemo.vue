<template>
  <div class="terminal">
    <div class="terminal-bar">
      <span class="dot dot-red"></span>
      <span class="dot dot-yellow"></span>
      <span class="dot dot-green"></span>
      <span class="terminal-title">harness9</span>
    </div>
    <div class="terminal-body">
      <pre class="terminal-pre">{{ output }}<span class="cursor" :class="{ blink: !typing }">▋</span></pre>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useData } from 'vitepress'

const SCRIPT_EN = `$ harness9

  HARNESS9  ·  claude-sonnet-4-6
  ctx: 0/128K (0%)  ·  new session

› help me analyze this project's code structure

  ✦ bash({"cmd":"find . -name '*.go' | head"}) — 18ms
  ✦ read_file({"path":"cmd/harness9/main.go"}) — 9ms

This is an Agent framework built on a standard ReAct architecture:

• cmd/harness9/ — TUI + CLI dual-mode entry point
• internal/engine/ — standard ReAct main loop
• internal/provider/ — OpenAI / Anthropic adapters
• internal/tools/ — built-in tool registry

› `

const SCRIPT_ZH = `$ harness9

  HARNESS9  ·  claude-sonnet-4-6
  ctx: 0/128K (0%)  ·  new session

› 帮我分析一下项目的代码结构

  ✦ bash({"cmd":"find . -name '*.go' | head"}) — 18ms
  ✦ read_file({"path":"cmd/harness9/main.go"}) — 9ms

这是一个标准 ReAct 架构的 Agent 框架：

• cmd/harness9/ — TUI + CLI 双模式入口
• internal/engine/ — 标准 ReAct 主循环
• internal/provider/ — OpenAI / Anthropic 适配器
• internal/tools/ — 内置工具注册表

› `

const { lang } = useData()
const SCRIPT = lang.value.startsWith('zh') ? SCRIPT_ZH : SCRIPT_EN

const output = ref('')
const typing = ref(true)
let timer: ReturnType<typeof setTimeout> | null = null
let charIndex = 0

function typeNext(): void {
  if (charIndex < SCRIPT.length) {
    output.value += SCRIPT[charIndex]
    charIndex++
    const delay = charIndex < 3 ? 80 : 22
    timer = setTimeout(typeNext, delay)
  } else {
    typing.value = false
    timer = setTimeout(reset, 2800)
  }
}

function reset(): void {
  output.value = ''
  charIndex = 0
  typing.value = true
  timer = setTimeout(typeNext, 600)
}

onMounted(() => {
  timer = setTimeout(typeNext, 1000)
})

onUnmounted(() => {
  if (timer) clearTimeout(timer)
})
</script>

<style scoped>
.terminal {
  width: 440px;
  border-radius: 10px;
  background: #0d1117;
  border: 1px solid #30363d;
  box-shadow: 0 24px 64px rgba(0, 0, 0, 0.6);
  font-family: 'Cascadia Code', 'Fira Code', 'JetBrains Mono', 'Menlo', monospace;
  font-size: 12.5px;
  overflow: hidden;
}

.terminal-bar {
  background: #161b22;
  padding: 10px 14px;
  display: flex;
  align-items: center;
  gap: 6px;
  border-bottom: 1px solid #30363d;
}

.dot {
  width: 12px;
  height: 12px;
  border-radius: 50%;
}

.dot-red    { background: #ff5f57; }
.dot-yellow { background: #febc2e; }
.dot-green  { background: #28c840; }

.terminal-title {
  flex: 1;
  text-align: center;
  color: #6e7681;
  font-size: 12px;
}

.terminal-body {
  padding: 14px 16px;
  min-height: 230px;
}

.terminal-pre {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
  color: #e6edf3;
  line-height: 1.65;
}

.cursor {
  color: #4ade80;
}

.cursor.blink {
  animation: blink 1s step-end infinite;
}

@keyframes blink {
  50% { opacity: 0; }
}
</style>
