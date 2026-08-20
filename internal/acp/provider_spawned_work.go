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
}

type providerSpawnedWorkTask struct {
	TaskID       string
	ToolCallID   string
	Description  string
	TaskType     string
	SubagentType string
	OutputFile   string
	Summary      string
	LastToolName string
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
