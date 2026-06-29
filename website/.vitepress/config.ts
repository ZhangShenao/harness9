import { defineConfig } from 'vitepress'
import { docsSidebar } from './sidebar.generated.js'

export default defineConfig({
  title: 'harness9',
  description: '轻量级、功能完备、生产可用的 Go Agent Harness 框架',
  base: '/harness9/',
  appearance: 'dark',
  themeConfig: {
    nav: [
      { text: '首页', link: '/' },
      { text: '文档', link: '/docs/quick-start' },
      { text: '博客', link: '/blog/' },
      {
        text: 'GitHub',
        link: 'https://github.com/ZhangShenao/harness9',
        target: '_blank',
      },
    ],
    sidebar: {
      '/docs/': docsSidebar,
      '/blog/': [
        {
          text: '技术博客',
          items: [
            { text: '所有文章', link: '/blog/' },
            { text: 'Agent Loop — 500 行 Go 代码驱动的生产级 ReAct 主循环', link: '/blog/agent-loop/' },
            { text: '工具调用系统 — 从接口契约到并发沙箱的工程实践', link: '/blog/tool-calling/' },
            { text: 'Planning 模块：Plan Mode、TodoStore 与执行自动化', link: '/blog/planning-module/' },
            { text: 'Agent Skill 系统 — Progressive Disclosure 思想下的 LLM 能力扩展协议', link: '/blog/agent-skills/' },
            { text: 'Context Engineering — 一个 Agent 如何在有限的 Token 窗口里保持清醒', link: '/blog/context-engineering/' },
            { text: 'Hooks 与 Human-in-the-Loop：harness9 的工具权限拦截体系', link: '/blog/hooks-human-in-the-loop/' },
          ],
        },
      ],
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/ZhangShenao/harness9' },
    ],
    search: {
      provider: 'local',
    },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Copyright © 2025-present ZhangShenao',
    },
  },
})
