# SWE-bench Lite Run Summary

- RunID: 20260910-171551
- 采样 seed: 1（同 seed 可复现同一实例集）
- 开始时间：2026-09-10 17:15:51
- 结束时间：2026-09-10 17:45:35
- 总实例数：3
- 成功生成 patch: 3 / 3
- 空 patch（agent 无改动）: 0
- 运行出错：0

## 按 Repo 分布

| Repo | 实例数 | 有 patch | 空 patch | 出错 |
|------|--------|---------|---------|------|
| django/django | 1 | 1 | 0 | 0 |
| pylint-dev/pylint | 1 | 1 | 0 | 0 |
| scikit-learn/scikit-learn | 1 | 1 | 0 | 0 |

## 评估命令

```bash
pip install swebench
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \
    --run_id harness9-lite-v1
```
