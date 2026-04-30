package event

// BudgetLimit names which budget axis tripped in BudgetExceeded.
type BudgetLimit string

const (
	LimitInputTokens  BudgetLimit = "input_tokens"
	LimitOutputTokens BudgetLimit = "output_tokens"
	LimitUSD          BudgetLimit = "usd"
	LimitWallClock    BudgetLimit = "wall_clock"
)

// BudgetWhere names where the trip was detected: before the provider
// call or while streaming chunks.
type BudgetWhere string

const (
	WherePreCall   BudgetWhere = "pre_call"
	WhereMidStream BudgetWhere = "mid_stream"
)

// RunErrorType classifies a RunFailed.
type RunErrorType string

const (
	RunErrorBudget   RunErrorType = "budget"
	RunErrorMaxTurns RunErrorType = "max_turns"
	RunErrorTool     RunErrorType = "tool"
	RunErrorProvider RunErrorType = "provider"
	RunErrorInternal RunErrorType = "internal"
)

// ToolErrorType classifies a ToolCallFailed.
type ToolErrorType string

const (
	ToolErrorPanic     ToolErrorType = "panic"
	ToolErrorCancelled ToolErrorType = "cancelled"
	ToolErrorTool      ToolErrorType = "tool"
)
