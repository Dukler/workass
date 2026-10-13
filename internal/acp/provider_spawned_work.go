package acp

// providerSpawnedWorkStrategy is the registered boundary for provider-owned
// background-work shapes. Raw tool/session payloads enter only this facet; the
// bridge and manager receive the typed observations below.
type providerSpawnedWorkStrategy interface {
	Supported() bool
	DecodeTool(providerRawToolObservation) (providerSpawnToolSignal, bool)
	DecodeLifecycle(any) (providerSpawnedWorkUpdate, bool)
	ValidateOutputPath(taskID, raw string) (string, bool)
}

type providerRawToolObservation struct {
	Title    string
	Command  string
	RawInput any
	Meta     map[string]any
	Output   string
}

type providerSpawnToolSignal struct {
	ProviderTool       string
	RunsInBackground   bool
	FallbackTaskID     string
	FallbackOutputFile string
	// ModelLabel is the model a subagent call asked for, when it named one
	// (Claude Code's Agent tool input "model"); the task events never carry it.
	ModelLabel string
}

type providerSpawnedWorkTask struct {
	TaskID string
	// ParentTaskID is the agent task that started this one, when an agent ran
	// its own background work (Claude Code's parent_task_id).
	ParentTaskID string
	ToolCallID   string
	Description  string
	TaskType     string
	SubagentType string
	OutputFile   string
	Summary      string
	LastToolName string
	ModelLabel   string
	Status       string
}

type providerSpawnedWorkUpdate struct {
	Kind       string
	TasksKnown bool
	Tasks      []providerSpawnedWorkTask
	Task       providerSpawnedWorkTask
}

type unsupportedProviderSpawnedWorkStrategy struct{}

func (unsupportedProviderSpawnedWorkStrategy) Supported() bool { return false }

func (unsupportedProviderSpawnedWorkStrategy) DecodeTool(providerRawToolObservation) (providerSpawnToolSignal, bool) {
	return providerSpawnToolSignal{}, false
}

func (unsupportedProviderSpawnedWorkStrategy) DecodeLifecycle(any) (providerSpawnedWorkUpdate, bool) {
	return providerSpawnedWorkUpdate{}, false
}

func (unsupportedProviderSpawnedWorkStrategy) ValidateOutputPath(string, string) (string, bool) {
	return "", false
}
