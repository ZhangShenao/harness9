package main

import "github.com/harness9/internal/mission"

// featureMissionGoal describes M2's frozen Feature Mission (design doc
// §2.1/§9.1 phase 3): a real, cross-package feature — full-text search over
// session messages — decomposed into a Task graph and driven end-to-end by
// real sub-agent LLM calls, proving the Scheduler/Worker/Verifier/Integration
// pipeline built in internal/mission, internal/scheduler, internal/worker,
// internal/verifier, and internal/integration works against a genuine
// feature rather than a synthetic fixture.
const featureMissionGoal = "为 harness9 新增会话全文检索能力：internal/memory 包新增 FTS5 全文检索方法，internal/tools 包新增 session_search 工具，并补充中英文文档"

// featureMissionPolicyJSON allows up to 3 Tasks to run concurrently, so the
// two independent implementation Tasks (memory-search, search-docs) can
// actually dispatch in parallel instead of serializing behind a
// max_concurrent_tasks=1 default.
const featureMissionPolicyJSON = `{"max_concurrent_tasks": 3}`

const memorySearchContract = `在 internal/memory 包中新增 FTS5 全文会话消息检索能力。

具体要求：
1. 新建文件 internal/memory/search.go。
2. 在 internal/memory/manager.go 的 schemaSQL 常量末尾追加一张 standalone FTS5 虚表（不要修改已有的 messages 表结构本身），保持幂等：
   CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(session_id UNINDEXED, role UNINDEXED, content)
3. 修改 internal/memory/sqlite_session.go 的 SQLiteSession.AddMessages 方法：在插入 messages 表的同一个事务里，把每条新消息同步写入 messages_fts（INSERT INTO messages_fts (session_id, role, content) VALUES (?, ?, ?)）。
4. 在 internal/memory/search.go 中新增：
   - 类型 MessageSearchResult struct，字段：SessionID string、Role string、Content string。
   - 方法 func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error)，用 FTS5 MATCH 语法在 messages_fts 表上检索，按确定性顺序排序（例如按 rowid 或 bm25() 排序均可，但必须是确定性的）。limit<=0 时默认最多返回 20 条。query 为空字符串时返回空切片和 nil error（不报错）。
5. 编写表驱动单元测试 internal/memory/search_test.go，至少覆盖：写入消息后能检索到、检索不存在的关键词返回空切片、limit 生效、多个 session 的消息互不串台。
6. 可以参考 internal/ltm/store.go 里 memories_fts 虚表的写法作为风格参照，但 internal/memory 包不应该 import internal/ltm（避免引入不必要的跨包依赖）。

不需要修改 cmd/harness9/main.go，不需要注册任何工具或补充文档，这些是其他 Task 的范围。`

const searchDocsContract = `为一个即将加入 harness9 的新功能——"会话全文检索"——补充项目文档。这个功能会新增：
- internal/memory 包里的 FTS5 全文检索能力：一张 standalone FTS5 虚表 messages_fts（镜像 messages 表的 session_id/role/content），新消息写入时同步索引；新方法 func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error)，用 FTS5 全文检索历史会话消息，MessageSearchResult 包含 SessionID/Role/Content 三个字符串字段。
- internal/tools 包里新增的 session_search 工具（BaseTool 实现），让 Agent 可以在对话中调用它检索历史会话消息，参数为 query（检索关键词，必填）和 limit（返回上限，可选，默认 5）。

具体要求：
1. 在 docs/核心功能/context-engineering.md 文件末尾追加一个新的二级标题小节（如"### 会话全文检索（FTS5）"），用中文说明这个新能力：解决什么问题（区别于 internal/ltm 对长期记忆条目的检索——这个是对会话历史原始消息本身的全文检索）、核心组件（messages_fts 虚表、SearchMessages 方法、session_search 工具）、大致工作原理（FTS5 + 写入时同步索引）。参考同文件里其他小节的写法风格和详略程度。
2. 在 docs/core-features-en/context-engineering.md 对应位置追加同样内容的英文版本（这个文件是中文文档的英文镜像，保持章节结构和位置一致）。
3. 只在这两个已有文件末尾追加内容，不需要新建文件。
4. 这是纯文档改动，不涉及代码改动，不需要写测试；但仍然按 Contract 要求的标准流程跑一遍 go build ./... 和 go test ./... -timeout 5m 确认没有破坏任何东西。

这个 Task 不需要等待其他 Task 完成——它和实际实现是并行进行的，上面描述的 API 签名和工具行为是最终确定的设计，直接依此撰写文档即可。`

const searchToolContract = `在 internal/tools 包中新增 session_search 工具，暴露 internal/memory 包新增的会话全文检索能力给 Agent 使用。

前置条件：internal/memory 包已经新增了 func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error) 方法，MessageSearchResult 有 SessionID/Role/Content 三个 string 字段。

具体要求：
1. 新建文件 internal/tools/session_search.go，参考 internal/tools/memory_search.go 的写法风格（工具结构体持有一个依赖、Name/Definition/Execute 三个方法、错误信息小写不加句号、用 encoding/json 序列化结果）。
2. 定义 SessionSearchTool struct，持有一个 *memory.Manager 字段。
3. func NewSessionSearchTool(manager *memory.Manager) *SessionSearchTool 构造函数。
4. Name() 返回 "session_search"。
5. Definition() 返回 schema.ToolDefinition，工具描述说明这是检索当前及历史会话原始消息内容的全文检索（区别于 memory_search 检索的是长期记忆条目，这个检索的是会话消息本身），参数 query（string，必填，检索关键词）和 limit（integer，可选，默认 5）。
6. Execute(ctx, args) 调用 manager.SearchMessages，把结果序列化为 JSON 数组字符串返回；无命中时返回 "[]"；query 为空时返回参数错误（不发起检索）。
7. 编写表驱动单元测试 internal/tools/session_search_test.go：用 t.TempDir() 下的文件路径构造一个真实的 *memory.Manager（memory.NewManager 的具体参数以 internal/memory 包已有测试的写法为准），打开一个 session 写入几条真实消息后再检索，覆盖：命中、未命中、limit 生效、query 为空时的参数错误。

不需要修改 cmd/harness9/main.go 里的工具注册——这留给后续接入 main.go 时处理，不在这个 Task 范围内。`

const verifyContractPlaceholder = "使用与被验证 Task 相同的目标：独立重跑 go build/vet/test 确认其产出真实可用。"

const integrationContract = "合并 memory-search、search-docs、search-tool 三个 Task 各自的分支，跑一次联合 build/vet/test 验证组合后的结果整体可用。"

// featureMissionTasks is the frozen Task graph for the Feature Mission.
// memory-search and search-docs are independent (dispatch in parallel);
// search-tool depends on memory-search; each implementation Task has its own
// paired verification Task; integration depends on all three implementation
// Tasks and only unblocks once each has reached succeeded (driven there by
// its own verifier), per queueReadyDependents' ContractKind-aware rule.
func featureMissionTasks() []mission.TaskInput {
	return []mission.TaskInput{
		{
			ClientID:     "memory-search",
			Position:     1,
			Title:        "internal/memory: FTS5 会话消息全文检索",
			Contract:     memorySearchContract,
			ContractKind: mission.ContractImplementation,
		},
		{
			ClientID:     "search-docs",
			Position:     2,
			Title:        "补充会话全文检索中英文文档",
			Contract:     searchDocsContract,
			ContractKind: mission.ContractImplementation,
		},
		{
			ClientID:     "search-tool",
			Position:     3,
			Title:        "internal/tools: session_search 工具",
			Contract:     searchToolContract,
			ContractKind: mission.ContractImplementation,
			Dependencies: []string{"memory-search"},
		},
		{
			ClientID:     "verify-memory-search",
			Position:     4,
			Title:        "独立验证 memory-search",
			Contract:     verifyContractPlaceholder,
			ContractKind: mission.ContractVerification,
			Dependencies: []string{"memory-search"},
		},
		{
			ClientID:     "verify-search-docs",
			Position:     5,
			Title:        "独立验证 search-docs",
			Contract:     verifyContractPlaceholder,
			ContractKind: mission.ContractVerification,
			Dependencies: []string{"search-docs"},
		},
		{
			ClientID:     "verify-search-tool",
			Position:     6,
			Title:        "独立验证 search-tool",
			Contract:     verifyContractPlaceholder,
			ContractKind: mission.ContractVerification,
			Dependencies: []string{"search-tool"},
		},
		{
			ClientID:     "integration",
			Position:     7,
			Title:        "合并三个分支并联合验证",
			Contract:     integrationContract,
			ContractKind: mission.ContractIntegration,
			Dependencies: []string{"memory-search", "search-docs", "search-tool"},
		},
	}
}
