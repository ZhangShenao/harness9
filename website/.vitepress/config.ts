import { defineConfig } from 'vitepress'
import { docsSidebarEn, docsSidebarZh } from './sidebar.generated.js'

export default defineConfig({
  title: 'harness9',
  description: 'A lightweight, complete, production-ready Go Agent Harness framework',
  base: '/harness9/',
  appearance: 'dark',
  themeConfig: {
    socialLinks: [
      { icon: 'github', link: 'https://github.com/ZhangShenao/harness9' },
    ],
    search: {
      provider: 'local',
      options: {
        locales: {
          zh: {
            translations: {
              button: {
                buttonText: '搜索',
                buttonAriaLabel: '搜索',
              },
              modal: {
                displayDetails: '显示详细列表',
                resetButtonTitle: '重置搜索',
                backButtonTitle: '关闭搜索',
                noResultsText: '没有结果',
                footer: {
                  selectText: '选择',
                  selectKeyAriaLabel: '输入',
                  navigateText: '导航',
                  navigateUpKeyAriaLabel: '上箭头',
                  navigateDownKeyAriaLabel: '下箭头',
                  closeText: '关闭',
                  closeKeyAriaLabel: 'esc',
                },
              },
            },
          },
        },
      },
    },
  },
  locales: {
    root: {
      label: 'English',
      lang: 'en',
      themeConfig: {
        nav: [
          { text: 'Home', link: '/' },
          { text: 'Docs', link: '/docs/quick-start' },
          { text: 'Blog', link: '/blog/' },
          {
            text: 'GitHub',
            link: 'https://github.com/ZhangShenao/harness9',
            target: '_blank',
          },
        ],
        sidebar: {
          '/docs/': docsSidebarEn,
          '/blog/': [
            {
              text: 'Technical Blog',
              items: [
                { text: 'All Posts', link: '/blog/' },
                { text: 'Agent Loop: A Production-Grade ReAct Loop in 500 Lines of Go', link: '/blog/agent-loop/' },
                { text: "harness9's Tool Calling System: From Interface Contracts to Concurrent Sandboxing", link: '/blog/tool-calling/' },
                { text: 'The Planning Module: Plan Mode, TodoStore, and Execution Automation', link: '/blog/planning-module/' },
                { text: 'The Agent Skills System: Extending LLM Capabilities via Progressive Disclosure', link: '/blog/agent-skills/' },
                { text: 'Context Engineering: How an Agent Stays Sane Within a Limited Token Window', link: '/blog/context-engineering/' },
                { text: 'Context Compaction Is Not Deleting History: How harness9 Keeps an Agent on Track', link: '/blog/progressive-context-compaction/' },
                { text: "Hooks and Human-in-the-Loop: harness9's Tool Permission Interception System", link: '/blog/hooks-human-in-the-loop/' },
                { text: 'Sub-Agent: How harness9 Lets the Main Agent Delegate Work', link: '/blog/sub-agent/' },
                { text: "Sandbox: Locking the Agent's Hands and Feet on a Floating Island", link: '/blog/sandbox/' },
                { text: 'Observability: Giving the Agent a Telescope Into Its Own Machinery', link: '/blog/observability/' },
                { text: 'Benchmarking Beyond Scores: How harness9 Iterates from Execution Traces', link: '/blog/benchmark-driven-iteration/' },
              ],
            },
          ],
        },
        footer: {
          message: 'Released under the MIT License.',
          copyright: 'Copyright © 2025-present ZhangShenao',
        },
      },
    },
    zh: {
      label: '简体中文',
      lang: 'zh-CN',
      link: '/zh/',
      title: 'harness9',
      description: '轻量级、功能完备、生产可用的 Go Agent Harness 框架',
      themeConfig: {
        nav: [
          { text: '首页', link: '/zh/' },
          { text: '文档', link: '/zh/docs/quick-start' },
          { text: '博客', link: '/zh/blog/' },
          {
            text: 'GitHub',
            link: 'https://github.com/ZhangShenao/harness9',
            target: '_blank',
          },
        ],
        sidebar: {
          '/zh/docs/': docsSidebarZh,
          '/zh/blog/': [
            {
              text: '技术博客',
              items: [
                { text: '所有文章', link: '/zh/blog/' },
                { text: 'Agent Loop — 500 行 Go 代码驱动的生产级 ReAct 主循环', link: '/zh/blog/agent-loop/' },
                { text: '工具调用系统 — 从接口契约到并发沙箱的工程实践', link: '/zh/blog/tool-calling/' },
                { text: 'Planning 模块：Plan Mode、TodoStore 与执行自动化', link: '/zh/blog/planning-module/' },
                { text: 'Agent Skill 系统 — Progressive Disclosure 思想下的 LLM 能力扩展协议', link: '/zh/blog/agent-skills/' },
                { text: 'Context Engineering — 一个 Agent 如何在有限的 Token 窗口里保持清醒', link: '/zh/blog/context-engineering/' },
                { text: '上下文压缩不是删历史 — harness9 怎样让 Agent 一直记得要做什么', link: '/zh/blog/progressive-context-compaction/' },
                { text: 'Hooks 与 Human-in-the-Loop：harness9 的工具权限拦截体系', link: '/zh/blog/hooks-human-in-the-loop/' },
                { text: 'Sub-Agent：harness9 如何让主代理把任务外包出去', link: '/zh/blog/sub-agent/' },
                { text: 'Sandbox：把 Agent 的手脚关进一座悬浮孤岛', link: '/zh/blog/sandbox/' },
                { text: 'Observability：给 Agent 装一台看清内部运转的望远镜', link: '/zh/blog/observability/' },
                { text: 'Benchmark 不只测分数 — harness9 如何用轨迹驱动迭代', link: '/zh/blog/benchmark-driven-iteration/' },
                { text: 'M1 完成之后：harness9 为什么要走向本地 Agent OS', link: '/zh/blog/m1-to-local-agent-os/' },
                { text: 'Agent 失控时，谁来踩刹车？', link: '/zh/blog/agent-loop-guardrails/' },
              ],
            },
          ],
        },
        footer: {
          message: '基于 MIT 协议发布。',
          copyright: 'Copyright © 2025-present ZhangShenao',
        },
      },
    },
  },
})
