// Package engine — PermissionMode 枚举与 WithPermissionMode 选项。
// 本文件定义工具执行的全局权限策略枚举。
package engine

// PermissionMode 控制工具执行的全局权限策略：危险操作是否需要审批。
type PermissionMode int

const (
	// PermissionModeDefault：不在白名单内的危险操作触发审批对话框（默认）。
	PermissionModeDefault PermissionMode = iota
	// PermissionModeAutoApprove：白名单内操作自动通过，其余仍需审批。
	PermissionModeAutoApprove
	// PermissionModeReadOnly：拒绝所有写操作（write_file / edit_file / bash 写命令）。
	PermissionModeReadOnly
	// PermissionModeBypassAll：绕过所有权限检查（危险，仅用于受控环境）。
	PermissionModeBypassAll
)

// String 返回 PermissionMode 的可读名称。
func (m PermissionMode) String() string {
	switch m {
	case PermissionModeDefault:
		return "default"
	case PermissionModeAutoApprove:
		return "auto-approve"
	case PermissionModeReadOnly:
		return "read-only"
	case PermissionModeBypassAll:
		return "bypass-all"
	default:
		return "unknown"
	}
}

// WithPermissionMode 设置 AgentEngine 的权限模式。
func WithPermissionMode(m PermissionMode) Option {
	return func(e *AgentEngine) { e.permissionMode = m }
}
