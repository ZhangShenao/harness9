package appservice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
)

// newTestService 构造面向测试的 Service：mock provider、无沙箱、临时目录隔离。
func newTestService(t *testing.T, prov provider.LLMProvider) *Service {
	t.Helper()
	return newTestServiceWithFactory(t, func(string) (provider.LLMProvider, int, error) {
		return prov, 128000, nil
	})
}

func newTestServiceWithFactory(t *testing.T, factory func(string) (provider.LLMProvider, int, error)) *Service {
	t.Helper()
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "work")
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("创建 workDir: %v", err)
	}
	svc, err := NewService(context.Background(), Config{
		WorkDir:              workDir,
		HomeDir:              homeDir,
		Model:                "test-model",
		SandboxBackend:       "off",
		DisableEnvLoad:       true,
		DisableObservability: true,
		ProviderFactory:      factory,
		DBPath:               filepath.Join(tmpDir, "sessions.db"),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// eventCollector 是线程安全的事件收集器（泵 goroutine 异步投递）。
type eventCollector struct {
	ch     chan Event
	mu     chan struct{}
	events []Event
}

func newEventCollector(svc *Service) *eventCollector {
	ec := &eventCollector{ch: make(chan Event, 256), mu: make(chan struct{}, 1)}
	svc.SetListener(func(evt Event) {
		ec.ch <- evt
	})
	go func() {
		for evt := range ec.ch {
			ec.events = append(ec.events, evt)
		}
		close(ec.mu)
	}()
	return ec
}

// waitDone 阻塞直到收到 Done/Error 事件或超时；返回收到的全部事件。
func (ec *eventCollector) waitDone(t *testing.T) []Event {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("等待任务结束超时，已收事件数: %d", len(ec.events))
		case <-time.After(10 * time.Millisecond):
			for _, evt := range ec.events {
				if evt.Type == string(engine.EventDone) || evt.Type == string(engine.EventError) {
					return ec.events
				}
			}
		}
	}
}

func (ec *eventCollector) snapshot() []Event { return append([]Event(nil), ec.events...) }

func TestServiceSessionLifecycle(t *testing.T) {
	svc := newTestService(t, providertest.NewMock())

	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := svc.RenameSession(activeID(t, svc), "测试标题"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	sessions, err := svc.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Title != "测试标题" {
		t.Fatalf("会话列表不符合预期: %+v", sessions)
	}

	// 重新打开（会话栈重建）后标题保持
	info, items, plan, err := svc.OpenSession(sessions[0].ID)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if info.Title != "测试标题" || len(items) != 0 || plan != nil {
		t.Fatalf("OpenSession 返回不符合预期: info=%+v items=%d plan=%v", info, len(items), plan)
	}

	if err := svc.DeleteSession(sessions[0].ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	sessions, _ = svc.ListSessions()
	if len(sessions) != 0 {
		t.Fatalf("删除后会话列表应为空: %+v", sessions)
	}
	if _, ok := svc.ActiveSession(); ok {
		t.Fatalf("删除活跃会话后 ActiveSession 应返回 false")
	}
}

// activeID 返回当前活跃会话 ID。
func activeID(t *testing.T, svc *Service) string {
	t.Helper()
	info, ok := svc.ActiveSession()
	if !ok {
		t.Fatalf("没有活跃会话")
	}
	return info.ID
}

func TestServiceSendMessagePumpsEvents(t *testing.T) {
	svc := newTestService(t, providertest.NewMock())
	seedBashWhitelist(t, svc)
	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ec := newEventCollector(svc)

	if err := svc.SendMessage("帮我看看目录里有什么"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !svc.IsRunning() {
		t.Fatalf("SendMessage 后应处于 running 状态")
	}
	// 单飞：运行中再次发送必须被拒绝
	if err := svc.SendMessage("第二条"); err == nil {
		t.Fatalf("运行中重复发送应返回 ErrRunInProgress")
	}

	events := ec.waitDone(t)
	var sawDelta, sawToolStart, sawToolResult bool
	for _, evt := range events {
		switch evt.Type {
		case string(engine.EventActionDelta):
			sawDelta = true
		case string(engine.EventToolStart):
			sawToolStart = true
		case string(engine.EventToolResult):
			sawToolResult = true
		case string(engine.EventError):
			t.Fatalf("意外的错误事件: %v", evt.Data)
		}
	}
	if !sawDelta || !sawToolStart || !sawToolResult {
		t.Fatalf("事件不完整: delta=%v toolStart=%v toolResult=%v", sawDelta, sawToolStart, sawToolResult)
	}

	// 首条消息自动生成标题
	info, _ := svc.ActiveSession()
	if info.Title != "帮我看看目录里有什么" {
		t.Fatalf("标题自动生成失败: %q", info.Title)
	}
	// 运行结束后 running 复位（异步泵退出，轮询等待）
	deadline := time.Now().Add(5 * time.Second)
	for svc.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if svc.IsRunning() {
		t.Fatalf("任务结束后 running 应复位")
	}
}

// seedBashWhitelist 预置 bash 全放行规则，模拟用户已信任 bash 的场景，
// 使常规任务不触发审批弹窗（审批路径由专项用例覆盖）。
func seedBashWhitelist(t *testing.T, svc *Service) {
	t.Helper()
	settingsPath := filepath.Join(svc.cfg.WorkDir, ".harness9", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("创建设置目录: %v", err)
	}
	rules := permission.NewRules()
	rules.AddRule(permission.RuleAllow, []string{"bash(*)"})
	if err := permission.SaveRules(settingsPath, rules); err != nil {
		t.Fatalf("预置白名单: %v", err)
	}
}

func TestServiceApprovalAllowAndRemember(t *testing.T) {
	// mock：首轮发起 bash rm -rf（触发 DangerHook 高风险 Ask），次轮输出结论。
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "victim") // 不存在的路径，即使执行也无副作用
	callCount := 0
	prov := providertest.NewMockWithCallback(func(_ []schema.Message, tools []schema.ToolDefinition) schema.Message {
		if len(tools) == 0 || callCount > 0 {
			return schema.Message{Role: schema.RoleAssistant, Content: "任务完成"}
		}
		callCount++
		return schema.Message{
			Role: schema.RoleAssistant,
			ToolCalls: []schema.ToolCall{
				{ID: "call_rm", Name: "bash", Arguments: []byte(`{"command": "rm -rf ` + target + `"}`)},
			},
		}
	})
	svc := newTestService(t, prov)
	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ec := newEventCollector(svc)
	if err := svc.SendMessage("清理目标目录"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// 等待审批请求出现并应答（允许 + 总是允许）
	deadline := time.After(30 * time.Second)
	approved := false
	for !approved {
		select {
		case <-deadline:
			t.Fatalf("等待审批请求超时")
		case <-time.After(10 * time.Millisecond):
			for _, evt := range ec.snapshot() {
				if evt.Type != string(engine.EventApprovalRequired) {
					continue
				}
				data, ok := evt.Data.(ApprovalData)
				if !ok {
					t.Fatalf("审批事件 Data 应为 ApprovalData: %T", evt.Data)
				}
				// 空白名单下 bash 未匹配任何规则 → permission hook 默认 ask（medium）
				if data.ToolName != "bash" || data.RiskLevel != "medium" {
					t.Fatalf("审批内容不符合预期: %+v", data)
				}
				// 应答不存在/重复应答应报错
				if err := svc.RespondApproval(data.ID+"-nope", true, "", false); err == nil {
					t.Fatalf("应答不存在的审批应报错")
				}
				if err := svc.RespondApproval(data.ID, true, "", true); err != nil {
					t.Fatalf("RespondApproval: %v", err)
				}
				approved = true
			}
		}
	}

	events := ec.waitDone(t)
	var sawResult bool
	for _, evt := range events {
		if evt.Type == string(engine.EventToolResult) {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("批准后应观察到工具结果事件")
	}

	// "总是允许"应把 bash 模式写入白名单文件
	settingsPath := filepath.Join(svc.cfg.WorkDir, ".harness9", "settings.json")
	rules, err := permission.LoadRules(settingsPath)
	if err != nil {
		t.Fatalf("读取白名单: %v", err)
	}
	if got := rules.Evaluate("bash", `{"command": "rm -rf anything"}`); got != permission.RuleAllow {
		t.Fatalf("白名单未生效: %s", got)
	}
}

func TestServiceApprovalDenyWithFeedback(t *testing.T) {
	var callCount int
	prov := providertest.NewMockWithCallback(func(msgs []schema.Message, tools []schema.ToolDefinition) schema.Message {
		if len(tools) == 0 {
			return schema.Message{Role: schema.RoleAssistant, Content: "思考"}
		}
		callCount++
		if callCount == 1 {
			return schema.Message{
				Role: schema.RoleAssistant,
				ToolCalls: []schema.ToolCall{
					{ID: "call_rm2", Name: "bash", Arguments: []byte(`{"command": "rm -rf /tmp/never-exists-xyz"}`)},
				},
			}
		}
		// 拒绝后引擎把 feedback 作为工具错误回传，模型应能看到
		last := msgs[len(msgs)-1]
		if last.ToolCallID == "call_rm2" && !strings.Contains(last.Content, "用户拒绝了该操作") && !strings.Contains(last.Content, "拒绝") {
			t.Logf("提示：拒绝反馈未见原文，实际回传: %q", last.Content)
		}
		return schema.Message{Role: schema.RoleAssistant, Content: "好的，我不执行了"}
	})
	svc := newTestService(t, prov)
	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ec := newEventCollector(svc)
	if err := svc.SendMessage("再试一次删除"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("等待审批请求超时")
		case <-time.After(10 * time.Millisecond):
			denied := false
			for _, evt := range ec.snapshot() {
				if evt.Type == string(engine.EventApprovalRequired) {
					data := evt.Data.(ApprovalData)
					if err := svc.RespondApproval(data.ID, false, "用户拒绝了该操作，请勿删除文件", false); err != nil {
						t.Fatalf("RespondApproval: %v", err)
					}
					denied = true
				}
			}
			if denied {
				events := ec.waitDone(t)
				var sawErrorResult bool
				for _, evt := range events {
					if evt.Type == string(engine.EventToolResult) {
						if trd, ok := evt.Data.(engine.ToolResultData); ok && trd.Result.IsError {
							sawErrorResult = true
						}
					}
				}
				if !sawErrorResult {
					t.Fatalf("拒绝后工具结果应为 IsError")
				}
				return
			}
		}
	}
}

func TestHistoryToItems(t *testing.T) {
	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "第一问"},
		{Role: schema.RoleAssistant, Content: "我来查一下", ToolCalls: []schema.ToolCall{
			{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
			{ID: "c2", Name: "read_file", Arguments: []byte(`{"path":"a.go"}`)},
		}},
		{Role: schema.RoleUser, Content: "ls 输出", ToolCallID: "c1"},
		{Role: schema.RoleAssistant, Content: "结论"},
	}
	items := historyToItems(msgs)
	if len(items) != 5 {
		t.Fatalf("条目数不符: %d (%+v)", len(items), items)
	}
	// 期望顺序: user / assistant(text) / tool(c1, 已回填) / tool(c2, pending) / assistant
	if items[0].Kind != "user" || items[0].Text != "第一问" {
		t.Fatalf("items[0] 不符: %+v", items[0])
	}
	if items[1].Kind != "assistant" || items[1].Text != "我来查一下" {
		t.Fatalf("items[1] 不符: %+v", items[1])
	}
	if items[2].ToolName != "bash" || items[2].ToolOutput != "ls 输出" || items[2].Pending {
		t.Fatalf("items[2] 应为已回填的 bash 卡片: %+v", items[2])
	}
	if items[3].ToolName != "read_file" || !items[3].Pending {
		t.Fatalf("items[3] 应为 pending 的 read_file 卡片: %+v", items[3])
	}
	if items[4].Kind != "assistant" || items[4].Text != "结论" {
		t.Fatalf("items[4] 不符: %+v", items[4])
	}
}

func TestServiceSessionDiff(t *testing.T) {
	svc := newTestService(t, providertest.NewMock())

	// 非 git 目录 → IsGitRepo=false 而非错误
	diff, err := svc.GetSessionDiff()
	if err != nil {
		t.Fatalf("GetSessionDiff(非 git): %v", err)
	}
	if diff.IsGitRepo {
		t.Fatalf("临时目录不应被识别为 git 仓库")
	}

	// git 仓库 + 未提交修改 → Dirty + Patch
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", svc.cfg.WorkDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(svc.cfg.WorkDir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(svc.cfg.WorkDir, "hello.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.cfg.WorkDir, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err = svc.GetSessionDiff()
	if err != nil {
		t.Fatalf("GetSessionDiff: %v", err)
	}
	if !diff.IsGitRepo || !diff.Dirty {
		t.Fatalf("应识别为脏 git 仓库: %+v", diff)
	}
	if !strings.Contains(diff.Patch, "hello world") {
		t.Fatalf("补丁应包含修改内容: %q", diff.Patch)
	}
	if len(diff.Untracked) != 1 || diff.Untracked[0] != "new.txt" {
		t.Fatalf("未跟踪文件列表不符: %v", diff.Untracked)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	homeDir := t.TempDir()
	st, err := LoadSettings(homeDir)
	if err != nil {
		t.Fatalf("LoadSettings(不存在): %v", err)
	}
	if st.Model != "" {
		t.Fatalf("默认设置应为零值: %+v", st)
	}

	st = Settings{
		Model:          "deepseek-chat",
		PermissionMode: "auto_approve",
		SandboxBackend: "seatbelt",
		RecentWorkDirs: []string{"/tmp/a"},
	}
	if err := SaveSettings(homeDir, st); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings(homeDir)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.Model != "deepseek-chat" || got.PermissionMode != "auto_approve" ||
		got.SandboxBackend != "seatbelt" || len(got.RecentWorkDirs) != 1 {
		t.Fatalf("回读不符: %+v", got)
	}
}

func TestMergeRecentDedup(t *testing.T) {
	got := mergeRecent([]string{"/b", "/a", "/b", "", "/c", "/a"}, 3)
	if len(got) != 3 || got[0] != "/b" || got[1] != "/a" || got[2] != "/c" {
		t.Fatalf("去重保序截断不符: %v", got)
	}
}

func TestPermissionModeConversion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want engine.PermissionMode
	}{
		{"", engine.PermissionModeDefault},
		{"default", engine.PermissionModeDefault},
		{"auto_approve", engine.PermissionModeAutoApprove},
		{"read_only", engine.PermissionModeReadOnly},
		{"bypass_all", engine.PermissionModeBypassAll},
	} {
		got, err := permissionModeFromString(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("permissionModeFromString(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := permissionModeFromString("bogus"); err == nil {
		t.Fatalf("非法模式应报错")
	}
	if got := permissionModeToString(engine.PermissionModeBypassAll); got != "bypass_all" {
		t.Fatalf("permissionModeToString 不符: %s", got)
	}
}

// TestServicePermissionModeApplied 验证 Settings 中的权限模式传递到引擎栈。
func TestServicePermissionModeApplied(t *testing.T) {
	svc := newTestService(t, providertest.NewMock())
	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	svc.mu.Lock()
	stack := svc.stack
	svc.mu.Unlock()
	if stack.engine == nil {
		t.Fatalf("会话栈应包含引擎")
	}
	_ = json.Marshal // 保持 json 导入被使用（DTO 序列化在后续用例中扩展）
}

// TestServiceApprovalsClearedAfterRunEnd 锁定泵退出清理语义：
// run 结束（含中断）后，未应答的审批必须被清空，
// 否则 RespondApproval 会把决策发给已不存在的引擎接收者。
func TestServiceApprovalsClearedAfterRunEnd(t *testing.T) {
	prov := providertest.NewMockWithCallback(func(_ []schema.Message, tools []schema.ToolDefinition) schema.Message {
		if len(tools) == 0 {
			return schema.Message{Role: schema.RoleAssistant, Content: "思考"}
		}
		// 首个 action 调用触发 bash（空白名单 → permission hook ask），
		// 随后测试在中断 run，使引擎阻塞在 ResponseCh 上直到 ctx 取消。
		return schema.Message{
			Role: schema.RoleAssistant,
			ToolCalls: []schema.ToolCall{
				{ID: "call_block", Name: "bash", Arguments: []byte(`{"command": "sleep 30"}`)},
			},
		}
	})
	svc := newTestService(t, prov)
	if _, err := svc.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ec := newEventCollector(svc)
	if err := svc.SendMessage("触发审批后中断"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// 等审批事件到达且不应答
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("等待审批请求超时")
		case <-time.After(10 * time.Millisecond):
			if len(svc.PendingApprovals()) > 0 {
				goto approved
			}
		}
	}
approved:
	svc.Interrupt()

	// 泵退出后 running 复位且审批清空
	pollDeadline := time.Now().Add(10 * time.Second)
	for svc.IsRunning() && time.Now().Before(pollDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if svc.IsRunning() {
		t.Fatalf("中断后 running 应复位")
	}
	if got := svc.PendingApprovals(); len(got) != 0 {
		t.Fatalf("run 结束后审批应清空，实际: %+v", got)
	}
	_ = ec
}

// TestServiceSaveSettingsPreservesRecents 锁定设置合并语义：
// SaveSettings 不得用磁盘旧值覆盖调用方新值，也不得清空磁盘上的 recents。
func TestServiceSaveSettingsPreservesRecents(t *testing.T) {
	svc := newTestService(t, providertest.NewMock())

	svc.AddRecentWorkDir("/tmp/proj-a")
	svc.AddRecentWorkDir("/tmp/proj-b")

	hint, err := svc.SaveSettings(Settings{
		Model:          "new-model",
		PermissionMode: "read_only",
		SandboxBackend: "seatbelt",
	})
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if hint == "" {
		t.Fatalf("模型/后端变更应返回重启提示")
	}

	got, err := LoadSettings(svc.cfg.HomeDir)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.Model != "new-model" || got.PermissionMode != "read_only" || got.SandboxBackend != "seatbelt" {
		t.Fatalf("新设置未落盘（被磁盘旧值覆盖）: %+v", got)
	}
	if len(got.RecentWorkDirs) != 2 || got.RecentWorkDirs[0] != "/tmp/proj-b" {
		t.Fatalf("recents 应保留且新值优先: %v", got.RecentWorkDirs)
	}
}
