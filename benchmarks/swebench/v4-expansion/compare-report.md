# SWE-bench 评测报告：moonshotai__kimi-k3.harness9-lite-v5

## 总体结果

- 提交实例：78
- Resolved：67/78（85.9%）
- 95% Wilson CI：[76.5%, 91.9%]
- Unresolved：7
- Empty patch：1
- Error：3

## 按 Repo 分布

| Repo | 实例数 | Resolved | Rate |
|------|-------:|---------:|-----:|
| astropy/astropy | 6 | 3 | 50% |
| django/django | 8 | 7 | 88% |
| matplotlib/matplotlib | 8 | 5 | 62% |
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
| Input Tokens | 408886 | 556966 | 1264195 |
| Output Tokens | 11392 | 13400 | 24191 |
| LLM 调用 | 28 | 35 | 67 |
| Turns | 28 | 34 | 64 |
| 时长（秒） | 504 | 715 | 1686 |

- plan_write 采用：2/78 实例（3%）
- 采用 plan_write：2 实例，Turns 中位数 40
- 未采用：76 实例，Turns 中位数 28

## v3 参考对照（⚠️ 模型不同，仅方向性参考，不可归因）

- 重叠实例（两轮 submitted 交集）：47
- 两轮均解决：31
- 仅本轮解决：9
- 仅 v3 解决：1

v3 独有解决（候选回归，逐条复核轨迹后再定性）：
- matplotlib__matplotlib-23562
