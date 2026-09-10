# SWE-bench 评测报告：report-v5-excl-23562

## 总体结果

- 提交实例：77
- Resolved：67/77（87.0%）
- 95% Wilson CI：[77.7%, 92.8%]
- Unresolved：7
- Empty patch：1
- Error：2

## 按 Repo 分布

| Repo | 实例数 | Resolved | Rate |
|------|-------:|---------:|-----:|
| astropy/astropy | 6 | 3 | 50% |
| django/django | 8 | 7 | 88% |
| matplotlib/matplotlib | 7 | 5 | 71% |
| mwaskom/seaborn | 4 | 3 | 75% |
| pallets/flask | 3 | 3 | 100% |
| psf/requests | 6 | 6 | 100% |
| pydata/xarray | 5 | 4 | 80% |
| pylint-dev/pylint | 6 | 5 | 83% |
| pytest-dev/pytest | 8 | 8 | 100% |
| scikit-learn/scikit-learn | 8 | 8 | 100% |
| sphinx-doc/sphinx | 8 | 7 | 88% |
| sympy/sympy | 8 | 8 | 100% |

## 效率指标

| 指标 | 中位数 | 均值 | P90 |
|------|-------:|-----:|----:|
| Input Tokens | 405336 | 557527 | 1264195 |
| Output Tokens | 11268 | 13260 | 24010 |
| LLM 调用 | 28 | 35 | 67 |
| Turns | 28 | 34 | 64 |
| 时长（秒） | 496 | 715 | 1686 |

- plan_write 采用：2/77 实例（3%）
- 采用 plan_write：2 实例，Turns 中位数 40
- 未采用：75 实例，Turns 中位数 27
