package tools

import (
	"context"
	"encoding/json"

	"github.com/harness9/internal/schema"
)

// BaseTool 定义了所有工具必须实现的核心接口（Tool Interface）。
// 每个工具需提供：唯一名称、JSON Schema 参数定义、以及同步执行逻辑。
// 框架通过此接口实现工具的多态调用，无需在引擎层了解具体工具的实现细节。
type BaseTool interface {
	// Name 返回工具的唯一标识符，用于 LLM 在 ToolCall 中引用此工具。
	// 例如 "bash"、"read_file"、"edit" 等。
	Name() string

	// Definition 返回工具的元信息（ToolDefinition），包含名称、描述和参数 JSON Schema。
	// 此信息会被转发给 LLM Provider，使模型了解工具的用途和参数格式。
	Definition() schema.ToolDefinition

	// Execute 执行工具逻辑。args 为 LLM 传递的原始 JSON 参数，
	// 由具体工具实现负责反序列化和校验。
	// 返回工具执行的文本输出，或失败时的错误信息。
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}
