# SWE-bench Lite Run Summary

- RunID: 20260909-130555
- 采样 seed: 1（同 seed 可复现同一实例集）
- 开始时间：2026-09-09 13:05:55
- 结束时间：2026-09-09 16:24:47
- 总实例数：78
- 成功生成 patch: 77 / 78
- 空 patch（agent 无改动）: 0
- 运行出错：1

## 按 Repo 分布

| Repo | 实例数 | 有 patch | 空 patch | 出错 |
|------|--------|---------|---------|------|
| astropy/astropy | 6 | 6 | 0 | 0 |
| django/django | 8 | 8 | 0 | 0 |
| matplotlib/matplotlib | 8 | 7 | 0 | 1 |
| mwaskom/seaborn | 4 | 4 | 0 | 0 |
| pallets/flask | 3 | 3 | 0 | 0 |
| psf/requests | 6 | 6 | 0 | 0 |
| pydata/xarray | 5 | 5 | 0 | 0 |
| pylint-dev/pylint | 6 | 6 | 0 | 0 |
| pytest-dev/pytest | 8 | 8 | 0 | 0 |
| scikit-learn/scikit-learn | 8 | 8 | 0 | 0 |
| sphinx-doc/sphinx | 8 | 8 | 0 | 0 |
| sympy/sympy | 8 | 8 | 0 | 0 |

## 评估命令

```bash
pip install swebench
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \
    --run_id harness9-lite-v1
```
