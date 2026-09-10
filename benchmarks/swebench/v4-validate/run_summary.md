# SWE-bench Lite Run Summary

- RunID: 20260910-103921
- 采样 seed: 1（同 seed 可复现同一实例集）
- 开始时间：2026-09-10 10:39:21
- 结束时间：2026-09-10 12:28:31
- 总实例数：47
- 成功生成 patch: 47 / 47
- 空 patch（agent 无改动）: 0
- 运行出错：0

## 按 Repo 分布

| Repo | 实例数 | 有 patch | 空 patch | 出错 |
|------|--------|---------|---------|------|
| astropy/astropy | 4 | 4 | 0 | 0 |
| django/django | 4 | 4 | 0 | 0 |
| matplotlib/matplotlib | 4 | 4 | 0 | 0 |
| mwaskom/seaborn | 4 | 4 | 0 | 0 |
| pallets/flask | 3 | 3 | 0 | 0 |
| psf/requests | 4 | 4 | 0 | 0 |
| pydata/xarray | 4 | 4 | 0 | 0 |
| pylint-dev/pylint | 4 | 4 | 0 | 0 |
| pytest-dev/pytest | 4 | 4 | 0 | 0 |
| scikit-learn/scikit-learn | 4 | 4 | 0 | 0 |
| sphinx-doc/sphinx | 4 | 4 | 0 | 0 |
| sympy/sympy | 4 | 4 | 0 | 0 |

## 评估命令

```bash
pip install swebench
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \
    --run_id harness9-lite-v1
```
