// Package planning — PlanWriter 接口定义。
package planning

// PlanWriter 将计划条目持久化为人类可读的计划文档。
// 定义在 planning 包（使用者侧），供 PlanWriteTool 依赖，避免循环导入。
type PlanWriter interface {
	Write(items []PlanItem) error
}
