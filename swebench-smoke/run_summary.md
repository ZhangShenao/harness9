# SWE-bench Lite Run Summary

- RunID: 20260909-115935
- 采样 seed: 1（同 seed 可复现同一实例集）
- 开始时间：2026-09-09 11:59:35
- 结束时间：2026-09-09 13:04:47
- 总实例数：24
- 成功生成 patch: 24 / 24
- 空 patch（agent 无改动）: 0
- 运行出错：0

## 按 Repo 分布

| Repo | 实例数 | 有 patch | 空 patch | 出错 |
|------|--------|---------|---------|------|
| astropy/astropy | 2 | 2 | 0 | 0 |
| django/django | 2 | 2 | 0 | 0 |
| matplotlib/matplotlib | 2 | 2 | 0 | 0 |
| mwaskom/seaborn | 2 | 2 | 0 | 0 |
| pallets/flask | 2 | 2 | 0 | 0 |
| psf/requests | 2 | 2 | 0 | 0 |
| pydata/xarray | 2 | 2 | 0 | 0 |
| pylint-dev/pylint | 2 | 2 | 0 | 0 |
| pytest-dev/pytest | 2 | 2 | 0 | 0 |
| scikit-learn/scikit-learn | 2 | 2 | 0 | 0 |
| sphinx-doc/sphinx | 2 | 2 | 0 | 0 |
| sympy/sympy | 2 | 2 | 0 | 0 |

## 评估命令

```bash
pip install swebench
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \
    --run_id harness9-lite-v1
```
