// Package subagent 实现 harness9 的子代理（Sub-Agent）系统。
//
// 子代理是「运行在隔离 Session 上的普通 AgentEngine 实例」：拥有独立 context、
// 受限工具集与派生权限，由主代理通过 task 工具委派任务。本包提供子代理定义、
// 定义注册表、运行器（Runner）与 task 工具。
package subagent

import (
	"fmt"
	"regexp"
)

// namePattern 约束子代理名称：小写字母/数字开头，后续可含连字符。
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// alwaysDeniedTools 永远从子代理工具集剥离的工具（无论白名单黑名单如何声明）：
// task 家族四工具是主 agent 的协调平面——子代理不得递归委派（防递归），
// 也不得操纵兄弟任务或查询主 tracker（防越权，spec §7.1）。
var alwaysDeniedTools = map[string]bool{
	"task":         true,
	"task_status":  true,
	"task_wait":    true,
	"task_control": true,
}

// SubAgentDefinition 描述一个子代理类型。
type SubAgentDefinition struct {
	Name            string   // 唯一标识（小写字母、数字、连字符）
	Description     string   // 写给 LLM 看的"何时使用我"（调度核心）
	SystemPrompt    string   // 子代理 system prompt
	Tools           []string // 工具白名单；nil/空 = 继承父全部
	DisallowedTools []string // 工具黑名单（先 deny 后 allow）
	Model           string   // 模型覆盖；"" = 继承父模型
	MaxTurns        int      // 最大轮数；0 = 继承引擎默认
	Skills          []string // 启动时预加载的 skill 名称
	Source          string   // "builtin" 或文件路径（诊断用）
}

// Validate 校验定义合法性。错误信息不以大写开头、不以句号结尾。
func (d SubAgentDefinition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("子代理 name 不能为空")
	}
	if !namePattern.MatchString(d.Name) {
		return fmt.Errorf("子代理 name %q 不合法（须匹配 ^[a-z0-9][a-z0-9-]*$）", d.Name)
	}
	if d.Description == "" {
		return fmt.Errorf("子代理 %q 的 description 不能为空", d.Name)
	}
	if d.SystemPrompt == "" {
		return fmt.Errorf("子代理 %q 的 system prompt 不能为空", d.Name)
	}
	return nil
}

// ResolveTools 给定全部可用工具名集合，返回子代理实际可用的工具名集合：
//  1. Tools 非空 → 取 Tools∩all；Tools 为空 → 取 all
//  2. 移除 DisallowedTools
//  3. 永远移除 task 家族四工具（alwaysDeniedTools：防递归 + 防越权操纵主 tracker，
//     无论是否在白名单，spec §7.1）
func (d SubAgentDefinition) ResolveTools(all []string) []string {
	allowed := make(map[string]bool, len(all))
	for _, t := range all {
		allowed[t] = true
	}

	var base []string
	if len(d.Tools) > 0 {
		for _, t := range d.Tools {
			if allowed[t] {
				base = append(base, t)
			}
		}
	} else {
		base = append(base, all...)
	}

	denied := make(map[string]bool, len(alwaysDeniedTools)+len(d.DisallowedTools))
	for k := range alwaysDeniedTools {
		denied[k] = true
	}
	for _, t := range d.DisallowedTools {
		denied[t] = true
	}

	result := make([]string, 0, len(base))
	for _, t := range base {
		if !denied[t] {
			result = append(result, t)
		}
	}
	return result
}
