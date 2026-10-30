package solver

// LoopEvent is one typed, operator-visible event from an executor turn. A turn
// begins with one model request, may contain assistant text and tool calls,
// and ends after its tool results. Result closes the outer solver cycle.
// Text contains only public assistant output or a concise status, never a
// provider's private reasoning state.
type LoopEvent struct {
	Kind        string `json:"kind"`
	MessageType string `json:"message_type"` // system | assistant | user | result
	Subtype     string `json:"subtype,omitempty"`
	Turn        int    `json:"turn"`
	Step        int    `json:"step"`
	MessageID   string `json:"message_id,omitempty"`
	ToolCallID  string `json:"tool_call_id,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Text        string `json:"text,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Final       bool   `json:"final,omitempty"`
	Tokens      int    `json:"tokens,omitempty"`
}
