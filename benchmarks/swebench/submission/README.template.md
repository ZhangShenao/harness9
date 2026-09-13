# {{MODEL}} on SWE-bench Lite — harness9 Agent Harness

> Run ID：{{RUN_ID}} ｜ 日期：{{DATE}} ｜ 模型：{{MODEL}}

harness9 是一个基于 Go 构建的开源 Agent Harness 框架（标准 ReAct 主循环 + Docker 沙箱隔离执行）。
本目录是对应 run `{{RUN_ID}}` 的官方提交 artifacts：`all_preds.jsonl`、
`logs/<instance_id>/`（patch.diff / report.json / test_output.txt.gz）与
`trajs/<instance_id>.md`（推理时同步生成的人类可读轨迹，非事后补写）。

## 复现说明

```bash
git clone <本仓 harness9 仓库>
cd harness9
cp .env.example .env   # 填入 API Key 与 LLM_MODEL={{MODEL}}
./benchmarks/swebench/run-official-lite.sh --output <run 目录>
```

- 采样 seed 固定为 1，同 seed 复现同一实例集；
- run 目录内 `model_snapshot.json` 是评测启动时从 OpenRouter 公开元数据端点
  抓取的模型版本快照，作为"该轮由哪个版本模型服务"的存证。

## 质量红线声明（官方 checklist 逐条对照）

1. **pass@1**：单轮单次采样，每实例只提交一份 model_patch；无 best@k、无多 rollout 择优。
   轨迹（trajs/）均为推理时同步生成的单一轨迹。

2. **不使用 PASS_TO_PASS / FAIL_TO_PASS 知识**：Agent 运行时上下文只注入
   issue 原文（problem_statement）与仓库代码；数据集中的 FAIL_TO_PASS /
   PASS_TO_PASS / test_patch 字段在 runner 加载后不进入 Agent 可见上下文，
   隐藏测试在评测阶段由官方 harness 注入。

3. **不使用 hints 字段**：hints_text 在 prompt 组装（prompt.go）中被排除，
   Agent 从未看到任何 issue 评论/提示文本。

4. **无 web 查答案，且有容器级防护**：Agent 沙箱通过 docker `--add-host`
   将 GitHub 系域名解析钉到 `0.0.0.0`（github.com / raw.githubusercontent.com /
   api.github.com / codeload.github.com / objects.githubusercontent.com /
   gist.github.com），容器内访问这些域名 DNS 即拒、实测不可达；该机制由
   `NetworkBlockedHosts` 配置注入（internal/sandbox/container.go），依赖自举所需
   的 PyPI 通道不受影响。轨迹中可复核：不存在任何对上游 issue / patch 页面的成功抓取。

## 已知缺口

若本 run 的 `EXPORT_MANIFEST.md` 列有缺失件（如旧 run 无 per-instance 测试输出），
以清单为准如实披露，未做任何事后伪造补齐。
