package inspector

import "encoding/json"

const (
	PluginID              = "local.sub2api.gpt-inspector"
	Version               = "0.1.4"
	Capability            = "openai.oauth.outbound_transport.v1"
	maxRounds             = 20
	maxOutput             = 2 * 1024 * 1024
	defaultTimeoutMinutes = 30
	maxTimeoutMinutes     = 120
)

type Account struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
}
type Model struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
}
type Selection struct {
	AccountID      int64    `json:"account_id"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Prompts        []string `json:"prompts"`
	Rounds         int      `json:"rounds"`
	TimeoutMinutes *int     `json:"timeout_minutes,omitempty"`
}
type Command struct {
	ID       string `json:"id"`
	Instance string `json:"instance"`
	IssuedAt int64  `json:"issued_at"`
	Action   string `json:"action"`
	Selection
	BatchID string `json:"batch_id,omitempty"`
	Item    int    `json:"item,omitempty"`
}
type Config struct {
	Command *Command `json:"command,omitempty"`
}
type Item struct {
	PromptID   string `json:"prompt_id"`
	Round      int    `json:"round"`
	State      string `json:"state"`
	SessionID  string `json:"session_id,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Parts      int    `json:"parts,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Error      string `json:"error,omitempty"`
}
type Batch struct {
	ID string `json:"id"`
	Selection
	AccountName string `json:"account_name"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Items       []Item `json:"items"`
	Error       string `json:"error,omitempty"`
}
type Record struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Latest    *Batch `json:"latest,omitempty"`
	Pending   *Batch `json:"pending,omitempty"`
}
type Answer struct {
	Prompt        Prompt           `json:"prompt"`
	Text          string           `json:"text"`
	Reasoning     string           `json:"reasoning,omitempty"`
	ResponseID    string           `json:"response_id,omitempty"`
	ReturnedModel string           `json:"returned_model,omitempty"`
	Usage         json.RawMessage  `json:"usage,omitempty"`
	Error         string           `json:"error,omitempty"`
	ErrorCode     string           `json:"error_code,omitempty"`
	SessionID     string           `json:"session_id,omitempty"`
	Attempts      []RequestAttempt `json:"attempts,omitempty"`
}
type RequestAttempt struct {
	SessionID     string          `json:"session_id"`
	ResponseID    string          `json:"response_id,omitempty"`
	ReturnedModel string          `json:"returned_model,omitempty"`
	Usage         json.RawMessage `json:"usage,omitempty"`
	DurationMS    int64           `json:"duration_ms"`
	Error         string          `json:"error,omitempty"`
	ErrorCode     string          `json:"error_code,omitempty"`
}
type StoredAccount struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Results   int    `json:"results"`
	BatchID   string `json:"batch_id,omitempty"`
	State     string `json:"state,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}
type Snapshot struct {
	Instance string          `json:"instance"`
	Ready    bool            `json:"ready"`
	Busy     string          `json:"busy,omitempty"`
	Error    string          `json:"error,omitempty"`
	Accounts []Account       `json:"accounts"`
	Prompts  []Prompt        `json:"prompts"`
	Stored   []StoredAccount `json:"stored"`
	Bytes    int64           `json:"bytes"`
	Results  int             `json:"results"`
	Active   []*Batch        `json:"active"`
	Revision uint64          `json:"revision"`
}
