package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is deliberately source-only. It is the Phase C build gate for
// ownership boundaries which are too important to leave to a runtime test:
// adding a new manager/session-store call must be an explicit source-review
// decision, and adding a new ChatID-bearing wire handler must enter the
// actor-facing ingress manifest below.

type phaseCSourceFile struct {
	path string
	set  *token.FileSet
	file *ast.File
}

type phaseCManagerCall struct {
	source   *phaseCSourceFile
	method   string
	function string
	pos      token.Pos
}

type phaseCHandler struct {
	source  *phaseCSourceFile
	channel string
	fn      *ast.FuncLit
}

func phaseCRepositoryRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(wd, "cmd", "workass", "main.go")); err == nil {
				return wd
			}
		}
		next := filepath.Dir(wd)
		if next == wd {
			break
		}
		wd = next
	}
	t.Fatal("could not locate the Workass repository root")
	return ""
}

func phaseCLoadSourceFiles(t *testing.T, repoRoot, relativeRoot string) []*phaseCSourceFile {
	t.Helper()
	root := filepath.Join(repoRoot, relativeRoot)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("source root %s: %v", relativeRoot, err)
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == ".dev" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", relativeRoot, err)
	}
	sort.Strings(paths)
	files := make([]*phaseCSourceFile, 0, len(paths))
	for _, path := range paths {
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, &phaseCSourceFile{path: path, set: set, file: file})
	}
	return files
}

func phaseCProductionFiles(t *testing.T) (string, []*phaseCSourceFile, []*phaseCSourceFile) {
	t.Helper()
	repoRoot := phaseCRepositoryRoot(t)
	cmdFiles := phaseCLoadSourceFiles(t, repoRoot, filepath.Join("cmd", "workass"))
	internalFiles := phaseCLoadSourceFiles(t, repoRoot, "internal")
	return repoRoot, cmdFiles, append(append([]*phaseCSourceFile(nil), cmdFiles...), internalFiles...)
}

func phaseCRelativePath(repoRoot, path string) string {
	relative, err := filepath.Rel(repoRoot, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func phaseCPosition(repoRoot string, source *phaseCSourceFile, pos token.Pos) string {
	position := source.set.Position(pos)
	return fmt.Sprintf("%s:%d", phaseCRelativePath(repoRoot, source.path), position.Line)
}

func phaseCFunctionName(source *phaseCSourceFile, pos token.Pos) string {
	name := "<file>"
	span := token.Pos(1<<62 - 1)
	for _, declaration := range source.file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || pos < function.Pos() || pos > function.End() {
			continue
		}
		currentSpan := function.End() - function.Pos()
		if currentSpan < span {
			span = currentSpan
			name = function.Name.Name
		}
	}
	return name
}

func phaseCReceiverType(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return phaseCReceiverType(value.X)
	case *ast.ParenExpr:
		return phaseCReceiverType(value.X)
	default:
		return ""
	}
}

func phaseCSessionStoreReceiver(expr ast.Expr) bool {
	return phaseCReceiverType(expr) == "sessionStore"
}

func phaseCSessionStoreExpression(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		switch value.Name {
		case "sessionStore", "sessionState", "sessions":
			return true
		}
	case *ast.SelectorExpr:
		switch value.Sel.Name {
		case "sessionStore", "sessionState", "sessions":
			return true
		}
	}
	return false
}

var phaseCAllowedSessionStoreMethods = map[string]struct{}{
	"enabled":                    {},
	"Get":                        {},
	"GlobalSnapshot":             {},
	"SaveActorGlobalSnapshot":    {},
	"SaveGlobalActiveTab":        {},
	"PersistProviderAttachments": {},
	"PlanProviderAttachments":    {},
	"ResolveProviderAttachment":  {},
	"LoadError":                  {},
	"publishedGeneration":        {},
	"recordLoadError":            {},
	"persistSnapshot":            {},
}

func TestPhaseCSessionStoreSurfaceIsExplicit(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	var violations []string
	for _, source := range cmdFiles {
		for _, declaration := range source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 || !phaseCSessionStoreReceiver(function.Recv.List[0].Type) {
				continue
			}
			if _, allowed := phaseCAllowedSessionStoreMethods[function.Name.Name]; !allowed {
				violations = append(violations, fmt.Sprintf("semantic sessionStore method %s at %s", function.Name.Name, phaseCPosition(repoRoot, source, function.Pos())))
			}
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !phaseCSessionStoreExpression(selector.X) {
				return true
			}
			if _, allowed := phaseCAllowedSessionStoreMethods[selector.Sel.Name]; !allowed {
				violations = append(violations, fmt.Sprintf("semantic sessionStore call %s at %s", selector.Sel.Name, phaseCPosition(repoRoot, source, call.Pos())))
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C sessionStore authority violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

var phaseCManagerReadOnlyMethods = map[string]struct{}{
	"Catalog":                        {},
	"CatalogSnapshotGroups":          {},
	"ConsumeRateLimitResetCredit":    {},
	"DetectProviders":                {},
	"KillAllProcesses":               {},
	"KillProcess":                    {},
	"Processes":                      {},
	"ProvidersList":                  {},
	"ReadProcess":                    {},
	"RefreshProviderPlanUsage":       {},
	"PublishProviderSnapshots":       {},
	"Reset":                          {},
	"SetModelScores":                 {},
	"SetProviderAttachmentPersister": {},
	"SetProviderAttachmentResolver":  {},
	"SetSessionRefreshFunc":          {},
	"SetChatEnvObserver":             {},
	"SetChatEnvRestorer":             {},
	"InstallSpawnedWorkObserver":     {},
	"StartProviderDetection":         {},
	"StartProviderStartup":           {},
	"StartProviderUpdate":            {},
	"StateDir":                       {},
	"Stats":                          {},
	"ToggleProvider":                 {},
}

type phaseCManagerRule struct {
	max  int
	note string
}

func phaseCManagerRules() map[string]phaseCManagerRule {
	return map[string]phaseCManagerRule{
		"cmd/workass/agent_control.go:ownerAuthorized:ValidateAgentOwner":                                         {1, "owner authorization after actor fencing"},
		"cmd/workass/agent_control.go:call:AgentCatalog":                                                          {1, "actor-authorized catalog projection"},
		"cmd/workass/chat_control.go:authorize:ValidateAgentOwner":                                                {1, "owner authorization after actor fencing"},
		"cmd/workass/chat_control.go:resolveControls:Catalog":                                                     {1, "provider catalog lookup"},
		"cmd/workass/main.go:registerAcpHandlers:LiveSession":                                                     {1, "known transient session lookup after actor route"},
		"cmd/workass/metrics.go:metrics:Stats":                                                                    {1, "global metrics projection"},
		"cmd/workass/provider_chat_agent_read.go:agentOwnerAuthorized:ValidateAgentOwner":                         {1, "owner authorization after actor fencing"},
		"cmd/workass/provider_chat_agent_read.go:WaitSubagent:WaitSubagent":                                       {1, "actor-owned wait receipt before transient observation"},
		"cmd/workass/provider_chat_agent_read.go:WaitSubagents:WaitSubagents":                                     {1, "actor-owned wait receipt before transient observation"},
		"cmd/workass/provider_chat_agent_read.go:ListSpawnedWorkForOwner:ReadSpawnedWork":                         {1, "bounded executor-cache enrichment"},
		"cmd/workass/provider_chat_agent_read.go:ListSpawnedWorkReceipts:ReadSpawnedWork":                         {1, "bounded executor-cache enrichment"},
		"cmd/workass/provider_chat_agent_read.go:reconcileAgentBackground:ListSpawnedWork":                        {1, "actor projection reconciliation"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:WithActorOwner":                          {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:SpawnSubagent":                           {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:MessageSubagent":                         {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:RetrySubagent":                           {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:CancelSubagent":                          {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:DecideSubagentPermission":                {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:RegisterExternalWork":                    {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:SettleExternalWork":                      {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:executeBackgroundAction:StopSpawnedWork":                         {1, "actor outbox executor bridge"},
		"cmd/workass/provider_chat_background.go:applySpawnedWorkSnapshot:ChatObligationEvidence":                 {1, "actor projection enrichment"},
		"cmd/workass/provider_chat_background.go:syncSpawnedWorkSnapshots:ListSpawnedWork":                        {1, "actor projection reconciliation"},
		"cmd/workass/provider_chat_background.go:syncSpawnedWorkSnapshots:CommitActorSpawnedWorkProjection":       {1, "persist the exact actor-owned executor liveness projection"},
		"cmd/workass/provider_chat_background.go:ReadBackground:ReadSpawnedWork":                                  {1, "bounded executor-cache enrichment"},
		"cmd/workass/provider_chat_close.go:CloseSession:LiveSessionByID":                                         {1, "known session-id compatibility lookup before actor proof"},
		"cmd/workass/provider_chat_runtime.go:newProviderChatRuntimeWithStartupMode:StoredProviderLaneSelections": {1, "read exact stored lane identity during actor schema conversion"},
		"cmd/workass/provider_chat_runtime.go:configureCoordinator:ForgetChat":                                    {1, "actor cleanup executor receives the exact operation identity"},
		"cmd/workass/provider_chat_runtime.go:configureCoordinator:RestoreChatCheckpoint":                         {1, "checkpoint executor receives the exact operation identity"},
		"cmd/workass/provider_chat_runtime.go:publishLifecycleReceipt:ChatEnvGet":                                 {1, "lifecycle projection enrichment"},
		"cmd/workass/provider_chat_runtime.go:restoreActorEnvironment:RestoreChatEnvReference":                    {1, "recovery attachment restore"},
		"cmd/workass/provider_chat_runtime.go:restoreActorChatEnvReference:RestoreChatEnvReference":               {1, "recovery attachment restore"},
		"cmd/workass/provider_chat_runtime.go:Select:LiveProviderLaneInfo":                                        {1, "provider attachment projection"},
		"cmd/workass/provider_chat_runtime.go:observeChatEnv:ChatCheckpoints":                                     {1, "environment projection enrichment"},
		"cmd/workass/provider_chat_runtime.go:observeChatEnv:ChatEnvReference":                                    {1, "environment projection enrichment"},
		"cmd/workass/provider_chat_runtime.go:ChatDiff:ChatDiffFromCheckpoints":                                   {1, "checkpoint projection read"},
		"cmd/workass/provider_chat_runtime.go:Fork:ResolveProviderLaneSelection":                                  {1, "known child selection before child actor attachment"},
		"cmd/workass/provider_chat_runtime.go:attachForkChild:LiveProviderLaneInfo":                               {1, "fork attachment projection"},
		"cmd/workass/provider_chat_runtime.go:resolveSelectionLocked:ResolveProviderLaneSelection":                {1, "actor selection command bridge"},
		"cmd/workass/provider_chat_runtime.go:selectLocked:ReattachProviderLane":                                  {1, "actor selection effect bridge"},
		"cmd/workass/provider_chat_runtime.go:admissionOutcomeLocked:ProviderLaneAdmission":                       {1, "actor admission receipt bridge"},
		"cmd/workass/provider_chat_runtime.go:Steer:LiveSessionByID":                                              {1, "post-commit transient-session recheck"},
		"cmd/workass/provider_chat_runtime.go:ResolvePermission:PermissionChatID":                                 {1, "permission request projection lookup"},
		"cmd/workass/provider_chat_runtime.go:reconcileObligations:ChatObligationEvidence":                        {1, "actor obligation projection enrichment"},
		"cmd/workass/stateless_mcp.go:ownerAuthorized:ValidateAgentOwner":                                         {1, "owner authorization after actor fencing"},
	}
}

func phaseCIsManagerReceiver(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name == "manager" || value.Name == "acpManager" || value.Name == "mgr" || value.Name == "managerRef"
	case *ast.SelectorExpr:
		return value.Sel.Name == "manager" || value.Sel.Name == "acpManager" || value.Sel.Name == "mgr" || value.Sel.Name == "managerRef"
	default:
		return false
	}
}

func phaseCManagerCallsInNode(source *phaseCSourceFile, node ast.Node) []phaseCManagerCall {
	var calls []phaseCManagerCall
	ast.Inspect(node, func(current ast.Node) bool {
		call, ok := current.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !phaseCIsManagerReceiver(selector.X) {
			return true
		}
		calls = append(calls, phaseCManagerCall{
			source: source, method: selector.Sel.Name,
			function: phaseCFunctionName(source, call.Pos()), pos: call.Pos(),
		})
		return true
	})
	return calls
}

func TestPhaseCManagerBoundaryIsExplicit(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	rules := phaseCManagerRules()
	counts := make(map[string]int)
	var violations []string
	for _, source := range cmdFiles {
		for _, call := range phaseCManagerCallsInNode(source, source.file) {
			if _, readOnly := phaseCManagerReadOnlyMethods[call.method]; readOnly {
				continue
			}
			relative := phaseCRelativePath(repoRoot, source.path)
			key := relative + ":" + call.function + ":" + call.method
			rule, allowed := rules[key]
			if !allowed {
				violations = append(violations, fmt.Sprintf("unreviewed manager call %s at %s", key, phaseCPosition(repoRoot, source, call.pos)))
				continue
			}
			counts[key]++
			if counts[key] > rule.max {
				violations = append(violations, fmt.Sprintf("manager call count exceeded for %s at %s (%s)", key, phaseCPosition(repoRoot, source, call.pos), rule.note))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C manager authority violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func TestPhaseCAgentWaitBindsOperationIDBeforeTransientManager(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	var controlSource *phaseCSourceFile
	var readSource *phaseCSourceFile
	for _, source := range cmdFiles {
		switch phaseCRelativePath(repoRoot, source.path) {
		case "cmd/workass/agent_control.go":
			controlSource = source
		case "cmd/workass/provider_chat_agent_read.go":
			readSource = source
		}
	}
	if controlSource == nil || readSource == nil {
		t.Fatal("agent wait source files are missing")
	}

	var controlCall *ast.FuncDecl
	for _, declaration := range controlSource.file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "call" || function.Body == nil {
			continue
		}
		controlCall = function
		break
	}
	if controlCall == nil {
		t.Fatal("agent control call ingress is missing")
	}
	var waitCalls []*ast.CallExpr
	ast.Inspect(controlCall.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && (selector.Sel.Name == "WaitSubagent" || selector.Sel.Name == "WaitSubagents") {
			waitCalls = append(waitCalls, call)
		}
		return true
	})
	if len(waitCalls) != 2 {
		t.Fatalf("agent control wait calls = %d, want one single and one many ingress", len(waitCalls))
	}
	for _, call := range waitCalls {
		if !phaseCNodeHasOperationID(call) {
			t.Fatalf("agent control wait call at %s drops operationID", phaseCPosition(repoRoot, controlSource, call.Pos()))
		}
	}

	for _, name := range []string{"WaitSubagent", "WaitSubagents"} {
		var function *ast.FuncDecl
		for _, declaration := range readSource.file.Decls {
			candidate, ok := declaration.(*ast.FuncDecl)
			if ok && candidate.Name.Name == name && candidate.Body != nil {
				function = candidate
				break
			}
		}
		if function == nil {
			t.Fatalf("provider chat %s ingress is missing", name)
		}
		managerMethod := name
		managerCalls := phaseCManagerCallsInNode(readSource, function.Body)
		var managerCall *phaseCManagerCall
		for index := range managerCalls {
			if managerCalls[index].method == managerMethod {
				managerCall = &managerCalls[index]
				break
			}
		}
		if managerCall == nil {
			t.Fatalf("provider chat %s has no transient Manager boundary", name)
		}
		var receiptPos token.Pos
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "recordAgentWaitObservation" && receiptPos == 0 {
				receiptPos = call.Pos()
			}
			return true
		})
		if receiptPos == 0 || receiptPos > managerCall.pos {
			t.Fatalf("provider chat %s reaches Manager before actor wait receipt at %s", name, phaseCPosition(repoRoot, readSource, managerCall.pos))
		}
	}
}

func TestPhaseCRecoveryExecutorCallbacksCarryOperationIdentity(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	var violations []string
	found := map[string]bool{"SetChatCleanup": false, "SetCheckpointRestoreExecutor": false}
	for _, source := range cmdFiles {
		if phaseCRelativePath(repoRoot, source.path) != "cmd/workass/provider_chat_runtime.go" {
			continue
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "SetChatCleanup" && selector.Sel.Name != "SetCheckpointRestoreExecutor") || len(call.Args) == 0 {
				return true
			}
			name := selector.Sel.Name
			found[name] = true
			callback, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				violations = append(violations, fmt.Sprintf("%s is not configured with a function callback at %s", name, phaseCPosition(repoRoot, source, call.Pos())))
				return true
			}
			parameter := false
			for _, field := range callback.Type.Params.List {
				for _, ident := range field.Names {
					if ident.Name == "operationID" {
						parameter = true
					}
				}
			}
			if !parameter {
				violations = append(violations, fmt.Sprintf("%s callback drops operationID at %s", name, phaseCPosition(repoRoot, source, callback.Pos())))
			}
			managerMethod := map[string]string{"SetChatCleanup": "ForgetChat", "SetCheckpointRestoreExecutor": "RestoreChatCheckpoint"}[name]
			managerCall := false
			ast.Inspect(callback.Body, func(current ast.Node) bool {
				manager, ok := current.(*ast.CallExpr)
				if !ok {
					return true
				}
				method, ok := manager.Fun.(*ast.SelectorExpr)
				if !ok || method.Sel.Name != managerMethod {
					return true
				}
				managerCall = true
				if !phaseCNodeHasOperationID(manager) {
					violations = append(violations, fmt.Sprintf("%s callback drops operationID at Manager call %s", name, phaseCPosition(repoRoot, source, manager.Pos())))
				}
				return true
			})
			if !managerCall {
				violations = append(violations, fmt.Sprintf("%s callback has no %s Manager boundary", name, managerMethod))
			}
			return true
		})
	}
	for name, present := range found {
		if !present {
			violations = append(violations, fmt.Sprintf("provider chat runtime is missing %s callback setup", name))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C recovery executor identity violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func phaseCStringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func phaseCRegisteredHandlers(source *phaseCSourceFile) []phaseCHandler {
	var handlers []phaseCHandler
	ast.Inspect(source.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Register" {
			return true
		}
		base, ok := selector.X.(*ast.Ident)
		if !ok || base.Name != "hub" {
			return true
		}
		channel, ok := phaseCStringLiteral(call.Args[0])
		if !ok {
			return true
		}
		fn, ok := call.Args[1].(*ast.FuncLit)
		if !ok {
			return true
		}
		handlers = append(handlers, phaseCHandler{source: source, channel: channel, fn: fn})
		return true
	})
	return handlers
}

func phaseCChatIDChannel(channel string) bool {
	return strings.HasPrefix(channel, "session:") ||
		strings.HasPrefix(channel, "chat:") ||
		strings.HasPrefix(channel, "app-chat:") ||
		strings.HasPrefix(channel, "spawned-work:") ||
		strings.HasPrefix(channel, "job:") ||
		channel == "visualize:host"
}

var phaseCExpectedChatHandlers = map[string]struct{}{
	"session:get": {}, "session:save": {},
	"chat:queue-replace": {}, "chat:create": {}, "chat:presentation-save": {}, "chat:runtime-controls-save": {}, "chat:delete": {},
	"chat:archive-append": {}, "chat:archive-load": {}, "chat:commands-get": {}, "chat:permission-decide": {}, "chat:permissions-pending": {},
	"chat:env-get": {}, "chat:checkpoints": {}, "chat:rewind": {}, "chat:diff": {},
	"app-chat:new-session": {}, "app-chat:refresh-plan-usage": {}, "app-chat:fork": {}, "app-chat:close-session": {},
	"app-chat:reset": {}, "app-chat:set-model": {}, "app-chat:set-mode": {}, "app-chat:steer": {}, "app-chat:use-rate-limit-reset": {}, "app-chat:detect-acp": {},
	"spawned-work:list": {}, "spawned-work:read": {}, "spawned-work:stop": {}, "job:start": {}, "job:cancel": {},
	"visualize:host": {},
}

var phaseCActorHandlerExceptions = map[string]struct{}{
	"app-chat:refresh-plan-usage":   {},
	"app-chat:reset":                {},
	"app-chat:set-model":            {},
	"app-chat:set-mode":             {},
	"app-chat:use-rate-limit-reset": {},
	"app-chat:detect-acp":           {},
}

var phaseCExpectedStubChannels = map[string]struct{}{
	"chat:kill-terminal": {},
	"chat:kill-command":  {},
}

func phaseCStubListLiteral(expr ast.Expr) bool {
	array, ok := expr.(*ast.ArrayType)
	if !ok {
		return false
	}
	element, ok := array.Elt.(*ast.Ident)
	return ok && element.Name == "stubDef"
}

func phaseCStubChannels(source *phaseCSourceFile) []string {
	var channels []string
	for _, declaration := range source.file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "daemonStubs" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok || !phaseCStubListLiteral(literal.Type) {
				return true
			}
			for _, element := range literal.Elts {
				definition, ok := element.(*ast.CompositeLit)
				if !ok || len(definition.Elts) == 0 {
					continue
				}
				if channel, ok := phaseCStringLiteral(definition.Elts[0]); ok {
					channels = append(channels, channel)
				}
			}
			return false
		})
	}
	return channels
}

func phaseCHasProviderChatCall(fn *ast.FuncLit) bool {
	found := false
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if base, ok := selector.X.(*ast.Ident); ok && base.Name == "providerChats" {
			found = true
			return false
		}
		return true
	})
	return found
}

func TestPhaseCChatIngressManifestIsComplete(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	seen := make(map[string]phaseCHandler)
	var violations []string
	for _, source := range cmdFiles {
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 1 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Register" {
				return true
			}
			base, ok := selector.X.(*ast.Ident)
			if !ok || base.Name != "hub" {
				return true
			}
			if _, literal := phaseCStringLiteral(call.Args[0]); !literal && phaseCFunctionName(source, call.Pos()) != "registerStub" {
				violations = append(violations, fmt.Sprintf("dynamic ChatID-capable hub registration at %s must remain behind registerStub", phaseCPosition(repoRoot, source, call.Pos())))
			}
			return true
		})
		for _, handler := range phaseCRegisteredHandlers(source) {
			if !phaseCChatIDChannel(handler.channel) {
				continue
			}
			if _, expected := phaseCExpectedChatHandlers[handler.channel]; !expected {
				violations = append(violations, fmt.Sprintf("unlisted ChatID-bearing hub handler %q at %s", handler.channel, phaseCPosition(repoRoot, source, handler.fn.Pos())))
				continue
			}
			if _, duplicate := seen[handler.channel]; duplicate {
				violations = append(violations, fmt.Sprintf("duplicate ChatID-bearing hub handler %q at %s", handler.channel, phaseCPosition(repoRoot, source, handler.fn.Pos())))
				continue
			}
			seen[handler.channel] = handler

			for _, call := range phaseCManagerCallsInNode(source, handler.fn) {
				if _, readOnly := phaseCManagerReadOnlyMethods[call.method]; readOnly {
					continue
				}
				if handler.channel != "app-chat:steer" || call.method != "LiveSession" {
					violations = append(violations, fmt.Sprintf("direct manager call %s from ChatID handler %q at %s", call.method, handler.channel, phaseCPosition(repoRoot, source, call.pos)))
				}
			}
			if _, exception := phaseCActorHandlerExceptions[handler.channel]; !exception && !phaseCHasProviderChatCall(handler.fn) {
				violations = append(violations, fmt.Sprintf("ChatID handler %q does not delegate to providerChats at %s", handler.channel, phaseCPosition(repoRoot, source, handler.fn.Pos())))
			}
		}
	}
	for channel := range phaseCExpectedChatHandlers {
		if _, found := seen[channel]; !found {
			violations = append(violations, fmt.Sprintf("expected ChatID handler %q is not registered as a literal hub handler", channel))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C ChatID ingress manifest violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func TestPhaseCChatIDStubIngressIsExplicit(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	seen := make(map[string]int)
	var violations []string
	for _, source := range cmdFiles {
		for _, channel := range phaseCStubChannels(source) {
			if !phaseCChatIDChannel(channel) {
				continue
			}
			seen[channel]++
			if _, expectedHandler := phaseCExpectedChatHandlers[channel]; expectedHandler {
				continue
			}
			if _, expectedStub := phaseCExpectedStubChannels[channel]; !expectedStub {
				violations = append(violations, fmt.Sprintf("unlisted ChatID-bearing daemon stub %q at %s", channel, phaseCRelativePath(repoRoot, source.path)))
			}
		}
	}
	for channel := range phaseCExpectedStubChannels {
		if seen[channel] != 1 {
			violations = append(violations, fmt.Sprintf("expected ChatID-bearing daemon stub %q must occur exactly once, found %d", channel, seen[channel]))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C ChatID stub ingress violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func TestPhaseCObsoleteTranscriptStoreIsAbsent(t *testing.T) {
	_, _, allFiles := phaseCProductionFiles(t)
	forbidden := []string{
		"appendChatArchive", "loadChatArchive", "loadChatArchiveRecords",
		"copyChatArchivePrefix", "normalizeArchivedSteerChronology",
	}
	var violations []string
	for _, source := range allFiles {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range forbidden {
			if strings.Contains(string(raw), token) {
				violations = append(violations, fmt.Sprintf("obsolete transcript-store symbol %s remains in %s", token, source.path))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("obsolete transcript-store violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func TestPhaseCObsoleteExecutorConversionAPIIsAbsent(t *testing.T) {
	_, _, allFiles := phaseCProductionFiles(t)
	var violations []string
	for _, source := range allFiles {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"SpawnedWorkPairsForMigration", "PruneSpawnedWorkForMigration"} {
			if strings.Contains(string(raw), token) {
				violations = append(violations, fmt.Sprintf("obsolete executor conversion API %s remains in %s", token, source.path))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("obsolete executor conversion API violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func TestPhaseCChatIDExecutorMutationInventoryIsExplicit(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	allowed := map[string]int{
		"cmd/workass/artifact_mutation.go:hostArtifact:RegisterForOperation":                 1,
		"cmd/workass/visualize.go:registerVisualizeHandler:RegisterCapturedHTMLForOperation": 1,
	}
	counts := make(map[string]int)
	var violations []string
	for _, source := range cmdFiles {
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			track := selector.Sel.Name == "RegisterCapturedHTML" || selector.Sel.Name == "RegisterCapturedHTMLForOperation" || selector.Sel.Name == "RegisterForOperation"
			if selector.Sel.Name == "Register" {
				if receiver, ok := selector.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "artifacts" {
					track = true
				}
			}
			if !track {
				return true
			}
			key := phaseCRelativePath(repoRoot, source.path) + ":" + phaseCFunctionName(source, call.Pos()) + ":" + selector.Sel.Name
			counts[key]++
			max, expected := allowed[key]
			if !expected {
				violations = append(violations, fmt.Sprintf("unreviewed ChatID executor mutation %s at %s", key, phaseCPosition(repoRoot, source, call.Pos())))
			} else if counts[key] > max {
				violations = append(violations, fmt.Sprintf("ChatID executor mutation count exceeded for %s at %s", key, phaseCPosition(repoRoot, source, call.Pos())))
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C ChatID executor mutation inventory violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

func phaseCNodeHasOperationID(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(current ast.Node) bool {
		switch value := current.(type) {
		case *ast.Ident:
			if value.Name == "operationID" {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if value.Sel.Name == "OperationID" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func TestPhaseCStatelessMCPMutationBoundaryIsExplicit(t *testing.T) {
	repoRoot, cmdFiles, _ := phaseCProductionFiles(t)
	allowed := map[string]int{
		"cmd/workass/browser_mcp.go:callBrowserMCPTool:invokeBrowserControlMutation": 1,
		"cmd/workass/stateless_mcp.go:callTool:executeBrowserMutation":               1,
		"cmd/workass/stateless_mcp.go:callTool:invokeBrowserControlMutation":         1,
		"cmd/workass/stateless_mcp.go:callTool:invokeBrowserControlReceipt":          1,
		"cmd/workass/stateless_mcp.go:callTool:callAgentMCPTool":                     1,
		"cmd/workass/artifact_mutation.go:hostArtifact:executeBrowserMutation":       1,
		"cmd/workass/visualize.go:registerVisualizeHandler:executeBrowserMutation":   4,
	}
	operationRequired := map[string]struct{}{
		"executeBrowserMutation":       {},
		"invokeBrowserControlMutation": {},
		"invokeBrowserControlReceipt":  {},
		"callAgentMCPTool":             {},
	}
	counts := make(map[string]int)
	var violations []string
	for _, source := range cmdFiles {
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch function := call.Fun.(type) {
			case *ast.Ident:
				name = function.Name
			case *ast.SelectorExpr:
				if function.Sel.Name == "executeBrowserMutation" {
					name = function.Sel.Name
				}
			}
			if _, tracked := operationRequired[name]; !tracked {
				return true
			}
			key := phaseCRelativePath(repoRoot, source.path) + ":" + phaseCFunctionName(source, call.Pos()) + ":" + name
			counts[key]++
			max, expected := allowed[key]
			if !expected {
				violations = append(violations, fmt.Sprintf("unreviewed stateless MCP executor boundary %s at %s", key, phaseCPosition(repoRoot, source, call.Pos())))
			} else if counts[key] > max {
				violations = append(violations, fmt.Sprintf("stateless MCP executor boundary count exceeded for %s at %s", key, phaseCPosition(repoRoot, source, call.Pos())))
			}
			if !phaseCNodeHasOperationID(call) {
				violations = append(violations, fmt.Sprintf("stateless MCP executor boundary %s drops stable operationID at %s", key, phaseCPosition(repoRoot, source, call.Pos())))
			}
			return true
		})
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Phase C stateless MCP boundary violation(s):\n%s", strings.Join(violations, "\n"))
	}
}

// This manifest is the Phase C ingress contract: only tools that can commit a
// durable or external side effect require a caller-stable logical operation id.
// JSON-RPC request ids are intentionally absent from this boundary.
func TestPhaseCStatelessMCPOperationManifest(t *testing.T) {
	agentMutations := map[string]struct{}{
		"workass_create_chat": {}, "workass_rename_chat": {}, "workass_configure_chat": {},
		"workass_focus_chat": {}, "workass_delete_chat": {}, "workass_send_chat_message": {},
		"workass_cancel_chat_turn": {}, "workass_host_artifact": {}, "workass_spawn_subagent": {},
		"workass_wait_subagent": {}, "workass_wait_subagents": {}, "workass_message_subagent": {},
		"workass_retry_subagent": {}, "workass_register_external_work": {}, "workass_settle_external_work": {},
		"workass_cancel_subagent": {}, "workass_decide_subagent_permission": {},
	}
	browserMutations := map[string]struct{}{
		"workass_browser_open": {}, "workass_browser_navigate": {}, "workass_browser_click": {},
		"workass_browser_type": {}, "workass_browser_scroll": {}, "workass_browser_key": {},
		"workass_browser_batch": {}, "workass_browser_history": {},
	}

	check := func(kind statelessMCPKind, tools []map[string]any, expected map[string]struct{}) {
		t.Helper()
		seen := make(map[string]struct{}, len(tools))
		for _, tool := range tools {
			name := toString(tool["name"])
			seen[name] = struct{}{}
			_, mutates := expected[name]
			if statelessMCPToolMutates(kind, name) != mutates {
				t.Errorf("%s tool %s disagrees with the mutation manifest", kind, name)
			}
			schema := mapFromAnyMain(tool["inputSchema"])
			properties := mapFromAnyMain(schema["properties"])
			required := make(map[string]struct{})
			if values, ok := schema["required"].([]string); ok {
				for _, value := range values {
					required[value] = struct{}{}
				}
			}
			if mutates {
				if _, ok := properties["operation_id"]; !ok {
					t.Errorf("%s mutation %s has no operation_id schema property", kind, name)
				}
				if _, ok := required["operation_id"]; !ok {
					t.Errorf("%s mutation %s does not require operation_id", kind, name)
				}
			} else if _, ok := properties["operation_id"]; ok {
				t.Errorf("%s read-only tool %s exposes an operation_id field", kind, name)
			}
		}
		for name := range expected {
			if _, ok := seen[name]; !ok {
				t.Errorf("%s mutation %s is missing from tools/list", kind, name)
			}
		}
	}
	check(agentMCPKind, agentMCPTools(), agentMutations)
	check(browserMCPKind, browserMCPTools(), browserMutations)

	statelessSource, err := os.ReadFile(filepath.Join(phaseCRepositoryRoot(t), "cmd", "workass", "stateless_mcp.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statelessSource), "stableMCPRequestOperationID") {
		t.Fatal("stateless MCP still contains a JSON-RPC transport-id operation fallback")
	}
}
