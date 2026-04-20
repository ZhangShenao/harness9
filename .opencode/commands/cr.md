---
name: cr
description: Use when the user invokes /cr, requests a code review, or before committing to verify correctness, security, and quality of new or modified code in the working tree.
---

# cr — Code Review

## Overview

对当前工作区所有新增或修改的代码执行详细的 Code Review，按严重级别输出问题，不做任何修改。

## 执行步骤

### 1. 收集变更范围

```bash
git status          # 查看新增 / 修改 / 删除的文件列表
git diff            # 未暂存的改动
git diff --cached   # 已暂存的改动
```

将两者合并视为本次 Review 的完整范围。

### 2. 逐文件审查

对每个变更文件，依次检查：

| 维度 | 检查点 |
|------|--------|
| **正确性** | 逻辑错误、边界条件、空值/类型异常 |
| **安全性** | SQL 注入、XSS、命令注入、敏感信息硬编码、不安全的默认值 |
| **可维护性** | 函数/类职责单一、命名清晰、不必要的复杂度 |
| **性能** | N+1 查询、不必要的循环、大对象复制 |
| **测试覆盖** | 关键路径是否有对应测试，新代码是否破坏现有测试 |
| **依赖安全** | 新引入的第三方包是否合理，版本是否锁定 |

### 3. 检查敏感文件

若 `git status` 中出现以下类型的文件，**单独标注**，不得进入后续提交：

- `.env`、`*.pem`、`*credentials*`、`*secret*`
- 包含明文密码、API Key 的配置文件

### 4. 输出 Review 报告

按以下格式输出，无问题的级别可省略：

```
## Code Review 报告

### 🔴 Critical（必须修复，阻断提交）
- `文件路径:行号` — 问题描述

### 🟡 Warning（建议修复）
- `文件路径:行号` — 问题描述

### 🔵 Suggestion（可选优化）
- `文件路径:行号` — 建议描述

### ✅ 总结
- 变更文件数：N
- 通过提交：是 / 否（存在 Critical 问题时为否）
```

## 注意事项

- 只审查，不修改代码
- Critical 问题存在时，明确说明**不建议提交**，等待用户确认修复
- 若变更集为空（`git status` 无输出），告知用户当前无需 Review
