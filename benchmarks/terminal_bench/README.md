# Terminal-Bench (Harbor) Pilot

harness9 接入 [Harbor](https://github.com/harbor-framework/harbor) 评测框架，跑
Terminal-Bench 2.0（`terminal-bench@2.0`，89 题）里精选的 18 个 pilot 任务。
背景与设计见 `docs/技术调研/terminal-bench-集成方案.md`。

## 环境准备

```bash
python3 --version   # 需要 >=3.12
pip install harbor
pip show harbor      # 确认 Home-page 指向 github.com/harbor-framework/harbor，
                      # 排除 PyPI 同名包冲突
docker --version     # Harbor 任务环境依赖 Docker
```

## 构建二进制

```bash
cd /Users/zsa/Desktop/harness/harness9
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -o benchmarks/terminal_bench/bin/harness9 \
  ./cmd/harness9
```

## 运行 Pilot

```bash
export OPENAI_API_KEY=...
# 可选：export OPENAI_BASE_URL=... （OpenRouter 等兼容端点）
# 可选：export LLM_MODEL=...

cd /Users/zsa/Desktop/harness/harness9
PYTHONPATH=benchmarks harbor run \
  -d terminal-bench@2.0 \
  -a terminal_bench.harness9_agent:Harness9Agent \
  -i fix-git -i git-multibranch -i configure-git-webserver \
  -i git-leak-recovery -i sanitize-git-repo \
  -i build-cython-ext -i build-pmars -i build-pov-ray -i compile-compcert \
  -i custom-memory-heap-crash -i fix-ocaml-gc -i merge-diff-arc-agi-task \
  -i sqlite-db-truncate -i sqlite-with-gcov -i nginx-request-logging \
  -i pypi-server -i kv-store-grpc -i openssl-selfsigned-cert \
  -o benchmarks/terminal_bench/runs
```

## 查看结果

跑完后先探查 `benchmarks/terminal_bench/runs/` 的真实目录结构（Harbor 的 `results.json` 字段
名未做源码级核实，不要凭猜测的字段名写脚本）：

```bash
find benchmarks/terminal_bench/runs -maxdepth 4 -type f
harbor job --help   # 查看是否有 status/summarize 一类的结果摘要子命令
```
