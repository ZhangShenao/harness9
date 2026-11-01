package appservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/harness9/internal/engine"
)

// Settings 是桌面端的用户偏好，持久化到 ~/.harness9/desktop/settings.json。
//
// 生效时机：
//   - PermissionMode：下一次会话栈构建（新建/打开会话）时生效——engine 按栈粒度构建；
//   - Model / SandboxBackend：重启 App 后生效（provider 与沙箱是跨会话共享的长生命周期资源）。
//
// API Key 等敏感信息不进本文件（沿用 .env / 系统环境变量，遵循项目安全约束 6.6）。
type Settings struct {
	// Model 模型名（对应 Config.Model）。
	Model string `json:"model,omitempty"`
	// PermissionMode 权限模式：default / auto_approve / read_only / bypass_all。
	PermissionMode string `json:"permission_mode,omitempty"`
	// SandboxBackend 沙箱后端：docker / seatbelt / off。
	SandboxBackend string `json:"sandbox_backend,omitempty"`
	// RecentWorkDirs 最近打开的工作目录（新任务选择器展示）。
	RecentWorkDirs []string `json:"recent_workdirs,omitempty"`
}

// SettingsPath 返回设置文件路径（<home>/.harness9/desktop/settings.json）。
func SettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".harness9", "desktop", "settings.json")
}

// LoadSettings 读取设置；文件不存在时返回零值设置而非错误（首次启动）。
func LoadSettings(homeDir string) (Settings, error) {
	data, err := os.ReadFile(SettingsPath(homeDir))
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{}, nil
		}
		return Settings{}, fmt.Errorf("读取设置：%w", err)
	}
	var st Settings
	if err := json.Unmarshal(data, &st); err != nil {
		return Settings{}, fmt.Errorf("解析设置：%w", err)
	}
	return st, nil
}

// SaveSettings 原子写入设置（0600，父目录 0700）。
func SaveSettings(homeDir string, st Settings) error {
	path := SettingsPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建设置目录：%w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化设置：%w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入设置：%w", err)
	}
	return os.Rename(tmp, path)
}

// GetSettings 返回当前生效的设置（活跃内存值，非磁盘回读）。
func (s *Service) GetSettings() Settings {
	return Settings{
		Model:          s.cfg.Model,
		PermissionMode: permissionModeToString(s.cfg.PermissionMode),
		SandboxBackend: s.cfg.SandboxBackend,
	}
}

// SaveSettings 持久化设置并把可热更的字段应用回 Config。
// 返回重启生效提示字段（model/sandbox 变更时非空）。
func (s *Service) SaveSettings(st Settings) (restartHint string, err error) {
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()

	if st.Model != "" && st.Model != cur.Model {
		cur.Model = st.Model
		restartHint = "模型变更将在重启应用后生效"
	}
	if st.PermissionMode != "" {
		mode, perr := permissionModeFromString(st.PermissionMode)
		if perr != nil {
			return "", perr
		}
		if mode != cur.PermissionMode {
			cur.PermissionMode = mode
			if restartHint == "" {
				restartHint = "权限模式将在下次打开会话时生效"
			}
		}
	}
	if st.SandboxBackend != "" {
		switch st.SandboxBackend {
		case "docker", "seatbelt", "off":
		default:
			return "", fmt.Errorf("非法的沙箱后端: %s", st.SandboxBackend)
		}
		if st.SandboxBackend != cur.SandboxBackend {
			cur.SandboxBackend = st.SandboxBackend
			if restartHint == "" {
				restartHint = "沙箱后端变更将在重启应用后生效"
			}
		}
	}

	s.mu.Lock()
	s.cfg = cur
	s.mu.Unlock()

	// 追加记录最近工作目录（去重、保序、上限 10）
	if len(st.RecentWorkDirs) > 0 {
		disk, err := LoadSettings(s.cfg.HomeDir)
		if err == nil {
			disk.RecentWorkDirs = mergeRecent(st.RecentWorkDirs, 10)
			st = disk
		}
	}
	if err := SaveSettings(s.cfg.HomeDir, st); err != nil {
		return "", err
	}
	return restartHint, nil
}

// AddRecentWorkDir 记录一次工作目录选择。
func (s *Service) AddRecentWorkDir(dir string) {
	st, err := LoadSettings(s.cfg.HomeDir)
	if err != nil {
		st = Settings{}
	}
	st.RecentWorkDirs = mergeRecent(append([]string{dir}, st.RecentWorkDirs...), 10)
	_ = SaveSettings(s.cfg.HomeDir, st)
}

// permissionModeToString / permissionModeFromString 是 PermissionMode 与
// 设置文件字符串的互转（设置文件用 snake_case 稳定契约，不暴露 Go 枚举）。
func permissionModeToString(m engine.PermissionMode) string {
	switch m {
	case engine.PermissionModeAutoApprove:
		return "auto_approve"
	case engine.PermissionModeReadOnly:
		return "read_only"
	case engine.PermissionModeBypassAll:
		return "bypass_all"
	default:
		return "default"
	}
}

func permissionModeFromString(s string) (engine.PermissionMode, error) {
	switch s {
	case "", "default":
		return engine.PermissionModeDefault, nil
	case "auto_approve":
		return engine.PermissionModeAutoApprove, nil
	case "read_only":
		return engine.PermissionModeReadOnly, nil
	case "bypass_all":
		return engine.PermissionModeBypassAll, nil
	default:
		return engine.PermissionModeDefault, fmt.Errorf("非法的权限模式: %s", s)
	}
}

// mergeRecent 去重保序并截断到 max 条。
func mergeRecent(dirs []string, max int) []string {
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
		if len(out) >= max {
			break
		}
	}
	return out
}
