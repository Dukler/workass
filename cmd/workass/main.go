package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	"workass/internal/fleet"
	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/machineid"
	"workass/internal/tlscert"
	"workass/internal/voice"
	"workass/internal/wire"
)

const daemonVersion = "0.0.1-dev"

var secretKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|bearer)`)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "browser-mcp" {
		if err := runBrowserMCPCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "workass browser-mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "fleet" {
		if err := runFleetCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "workass fleet: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "agent-mcp" {
		if err := runAgentMCPCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "workass agent-mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}
	prod := flag.Bool("prod", prodModeDefault(), "use production daemon defaults where applicable; on Windows this defaults --port 80 and --bind lan")
	port := flag.Int("port", 8788, "HTTP/WebSocket port")
	bind := flag.String("bind", "localhost", "bind mode: localhost or lan")
	beacon := flag.Bool("beacon", beaconDefault(), "announce this machine to others on the LAN; only applies under --bind lan")
	useTLS := flag.Bool("tls", false, "serve https/wss with this machine's own certificate, minted once into the state dir")
	rendererDir := flag.String("renderer-dir", "", "renderer directory override; empty serves embedded renderer2")
	mocksDirFlag := flag.String("mocks-dir", "", "design mocks directory override")
	acpCommand := flag.String("acp-command", "node", "ACP provider command")
	acpArgsJSON := flag.String("acp-args", `["desktop/acp/mock-server.mjs"]`, "ACP provider args as a JSON array")
	stateDirFlag := flag.String("state-dir", "state", "daemon state directory")
	trustLocalhost := flag.Bool("trust-localhost", true, "auto-approve localhost WebSocket clients")
	hibernateTTL := flag.Duration("hibernate-ttl", 20*time.Minute, "idle ACP chat hibernate TTL; accepts Go durations such as 20m or 250ms")
	rssSampleInterval := flag.Duration("rss-sample-interval", 30*time.Second, "ACP engine RSS sampling interval")
	engineMaxAge := flag.Duration("engine-max-age", 12*time.Hour, "ACP engine age recycle threshold")
	engineMaxRSSKB := flag.Int("engine-max-rss-kb", 4*1024*1024, "ACP engine RSS recycle threshold in KiB")
	spareSessions := flag.Int("spare-sessions", 0, "pre-warmed ACP spare sessions to keep ready, 0-4")
	spareTTL := flag.Duration("spare-ttl", 5*time.Minute, "pre-warmed spare ACP session TTL")
	compactionEnabled := flag.Bool("compaction-enabled", true, "enable ACP auto-compaction at turn boundaries")
	compactionThresholdPct := flag.Int("compaction-threshold-pct", 80, "ACP auto-compaction threshold as context usage percent")
	compactionKeepLastTurns := flag.Int("compaction-keep-last-turns", 4, "conversation turns to keep verbatim after ACP auto-compaction")
	flag.Parse()
	acpOverride := false
	flagOverrides := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		flagOverrides[f.Name] = true
		if f.Name == "acp-command" || f.Name == "acp-args" {
			acpOverride = true
		}
	})
	applyProductionDefaults(runtime.GOOS, *prod, flagOverrides, port, bind)

	if *bind != "localhost" && *bind != "lan" {
		fmt.Fprintln(os.Stderr, `invalid --bind: use "localhost" or "lan"`)
		os.Exit(2)
	}
	if *port <= 0 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "invalid --port: use 1-65535")
		os.Exit(2)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workass: get cwd: %v\n", err)
		os.Exit(1)
	}
	staticDir := *rendererDir
	if staticDir != "" && !filepath.IsAbs(staticDir) {
		staticDir = filepath.Join(cwd, staticDir)
	}
	stateDir := *stateDirFlag
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(cwd, stateDir)
	}
	mocksDir := resolveMocksDir(*mocksDirFlag, os.Getenv("WORKASS_MOCKS_DIR"), cwd, currentExecutablePath())
	flagEngine := engineConfig{
		HibernateTTL:            *hibernateTTL,
		MaxRSSKB:                *engineMaxRSSKB,
		MaxAge:                  *engineMaxAge,
		SpareSessions:           *spareSessions,
		RSSSampleInterval:       *rssSampleInterval,
		CompactionEnabled:       *compactionEnabled,
		CompactionThresholdPct:  *compactionThresholdPct,
		CompactionKeepLastTurns: *compactionKeepLastTurns,
	}
	appConfigPath := filepath.Join(filepath.Dir(stateDir), "app-config.json")
	appConfigStore := newAppConfigStore(appConfigPath, flagEngine, flagOverrides)
	if err := appConfigStore.ensureFile(); err != nil {
		fmt.Fprintf(os.Stderr, "workass: initialize config: %v\n", err)
		os.Exit(1)
	}
	effectiveEngine := appConfigStore.effectiveEngine()
	providersFile := filepath.Join(filepath.Dir(stateDir), "providers.json")
	providers, err := acp.LoadProviderConfigs(providersFile, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workass: load providers: %v\n", err)
		os.Exit(1)
	}
	defaultProviderID := ""
	if acpOverride {
		acpArgs, err := parseStringArrayFlag(*acpArgsJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --acp-args: %v\n", err)
			os.Exit(2)
		}
		custom := acp.ProviderConfig{
			ID:      "custom",
			Name:    "Custom ACP",
			Command: *acpCommand,
			Args:    acpArgs,
			Enabled: true,
			Badge:   "custom",
			CWD:     cwd,
		}
		providers = replaceProviderConfig(providers, custom)
		defaultProviderID = "custom"
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	leaseManager, err := lease.NewManager(lease.Options{
		StateDir: stateDir,
		Logf:     logger.Printf,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "workass: load device state: %v\n", err)
		os.Exit(1)
	}
	hub := wire.NewHub(wire.Options{
		Lease:          leaseManager,
		TrustLocalhost: *trustLocalhost,
		Logf:           logger.Printf,
	})
	sessionState := sharedSessionStore(stateDir)
	if err := sessionState.LoadError(); err != nil {
		logger.Printf("[workass] load session state %s: %v", filepath.Join(stateDir, sessionStateFilename), err)
	}
	agentControlFile := filepath.Join(stateDir, "agent-control.json")
	acpManager := acp.NewManager(acp.Options{
		RootDir:                 cwd,
		StateDir:                stateDir,
		RuntimeProfile:          workassRuntimeProfile(),
		BrowserMCPCommand:       currentExecutablePath(),
		BrowserMCPControlFile:   defaultBrowserControlFile(stateDir),
		AgentMCPCommand:         currentExecutablePath(),
		AgentMCPControlFile:     agentControlFile,
		Version:                 daemonVersion,
		Providers:               providers,
		DefaultProviderID:       defaultProviderID,
		ProviderConfigFile:      providersFile,
		Broadcast:               daemonEventBroadcaster(sessionState, hub.Broadcast),
		HibernateTTL:            effectiveEngine.HibernateTTL,
		RSSSampleInterval:       effectiveEngine.RSSSampleInterval,
		EngineMaxAge:            effectiveEngine.MaxAge,
		EngineMaxRSSKB:          effectiveEngine.MaxRSSKB,
		SpareSessions:           effectiveEngine.SpareSessions,
		SpareTTL:                *spareTTL,
		CompactionEnabled:       effectiveEngine.CompactionEnabled,
		CompactionThresholdPct:  effectiveEngine.CompactionThresholdPct,
		CompactionKeepLastTurns: effectiveEngine.CompactionKeepLastTurns,
		Logf: func(message string, fields map[string]any) {
			data, _ := json.Marshal(redactValue(fields))
			logger.Printf("[acp] %s %s", message, data)
		},
	})
	chatControl := newChatControlCoordinator(acpManager, sessionState, hub.Broadcast)
	channelCount := registerDaemonHandlers(hub, cwd, acpManager, daemonOptions{
		StateDir:            stateDir,
		ConfigPath:          appConfigPath,
		Engine:              effectiveEngine,
		EngineFlagOverrides: flagOverrides,
		ChatControl:         chatControl,
	})
	acpManager.StartProviderDetection(context.Background())

	host := "127.0.0.1"
	if *bind == "lan" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(*port))
	rendererSource := "embedded renderer2"
	if staticDir != "" {
		rendererSource = staticDir
	}
	logger.Printf("[workass] registered %d daemon wire channels", channelCount)
	scheme := "http"
	if *useTLS {
		scheme = "https"
	}
	logger.Printf("[workass] serving renderer %s on %s://%s", rendererSource, scheme, addr)
	if *bind == "lan" && !*useTLS {
		// The insecure window is something you can see rather than something you
		// have to remember. Without --tls, everything on this port is readable by
		// anyone on the network — prompts, agent output, the code in a diff, and
		// the device token that grants control of it all.
		logger.Printf("[workass] WARNING: bound to the LAN in the clear — http/ws, no TLS. " +
			"Prompts, agent output, diffs and device tokens are readable by anyone on this network. Use --tls.")
	}
	logger.Printf("[workass] device state %s (trust-localhost=%v)", filepath.Join(stateDir, "devices.json"), *trustLocalhost)
	logger.Printf("[workass] provider registry %s", providersFile)

	artifactHosting, err := artifacthost.New(stateDir, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		acpManager.Reset()
		logger.Printf("[workass] initialize artifact hosting: %v", err)
		os.Exit(1)
	}
	artifactHosting.SetLogger(logger.Printf)
	handler := httpserve.New(staticDir, hub, nil)
	handler.MocksDir = mocksDir
	handler.ArtifactHosts = artifactHosting
	if staticDir == "" {
		handler.RendererFS = embeddedRendererFS()
	}
	handler.Version = daemonVersion
	identity, identityErr := machineid.Load(stateDir)
	if identityErr != nil {
		// A daemon with no provable identity still serves its own client; it
		// just cannot be paired with or announced, so say so rather than
		// minting a replacement id behind the user's back.
		logger.Printf("[workass] machine identity unavailable: %v", identityErr)
	} else {
		logger.Printf("[workass] machine %s (%s)", identity.MachineID, identity.DisplayName)
	}
	// The fleet key is loaded, never minted here. A daemon that starts fresh must
	// not invent a fleet of one behind your back: `workass fleet key` mints, and
	// `workass fleet join` accepts the key another machine already holds.
	// Filled in below if --tls is on; the identity closure reads it every call,
	// so health tells the truth from the first request either way.
	tlsFingerprint := ""
	hub.SetMachineID(identity.MachineID)
	fleetKeys, fleetErr := fleet.Open(stateDir)
	switch {
	case fleetErr != nil:
		logger.Printf("[workass] fleet keys unavailable: %v", fleetErr)
	case fleetKeys.Has():
		hub.SetFleet(fleetKeys, identity.MachineID)
		logger.Printf("[workass] fleet %s", strings.Join(fleetKeys.KeyIDs(), ", "))
	default:
		hub.SetFleet(fleetKeys, identity.MachineID)
		logger.Printf("[workass] no fleet key yet; run `workass fleet key` to mint one or `workass fleet join` to use another machine's")
	}
	handler.Identity = func() map[string]any {
		return daemonIdentity(identity, os.Getenv(profileEnvVar), *bind, *port, acpManager, fleetKeys, tlsFingerprint)
	}
	machineBook := openMachineBook(stateDir, identity, logger)
	if machineBook != nil {
		logger.Printf("[workass] registered %d machine wire channels", registerMachineHandlers(hub, machineBook, identity))
	}
	handler.Metrics = func() map[string]any { return daemonMetrics(sessionState, stateDir, hub, acpManager) }
	controlURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)) + agentControlPath
	agentControl, err := newAgentControlHandler(acpManager, sessionState, hub.Broadcast, controlURL, agentControlFile, chatControl)
	if err != nil {
		acpManager.Reset()
		logger.Printf("[workass] initialize agent control: %v", err)
		os.Exit(1)
	}
	agentControl.artifacts = artifactHosting
	acpManager.SetSpawnedWorkWakeFunc(func(tabID, chatID string, items []acp.SpawnedWorkItem) error {
		return agentControl.chats.EnqueueServerNotice(tabID, chatID, spawnedWorkServerNoticeText(items))
	})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			acpManager.Reset()
			_ = os.Remove(agentControlFile)
		})
	}
	defer cleanup()
	mux := http.NewServeMux()
	mux.Handle(agentControlPath, agentControl)
	mux.Handle(fleetQRPath, newFleetQRHandler(fleetKeys, *port, logger.Printf))
	mux.Handle("/", handler)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		cleanup()
		logger.Printf("[workass] listen failed: %v", err)
		os.Exit(1)
	}
	server := &http.Server{Handler: mux}
	// E5. TLS is not a port: the daemon keeps listening exactly where the
	// firewall already allows it, and only the bytes change.
	if *useTLS {
		certificate, certErr := tlscert.Ensure(stateDir)
		if certErr != nil {
			cleanup()
			logger.Printf("[workass] tls: %v", certErr)
			os.Exit(1)
		}
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate.TLS},
			MinVersion:   tls.VersionTLS12,
		}
		listener = tls.NewListener(listener, server.TLSConfig)
		hub.SetTLSFingerprint(certificate.Fingerprint)
		tlsFingerprint = certificate.Fingerprint
		if certificate.Minted {
			logger.Printf("[workass] minted this machine's certificate in %s", stateDir)
		}
		logger.Printf("[workass] serving https/wss · certificate %s", certificate.Fingerprint[:16])
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	startMachinePresence(signalCtx, machineBook, hub, identity, machinePresenceOptions{
		Bind:   *bind,
		Port:   *port,
		Beacon: *beacon,
	}, logger)
	if err := serveDaemonHTTP(signalCtx, server, listener, cleanup); err != nil {
		cleanup()
		logger.Printf("[workass] server stopped: %v", err)
		os.Exit(1)
	}
}

func serveDaemonHTTP(ctx context.Context, server *http.Server, listener net.Listener, cleanup func()) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if cleanup != nil {
			cleanup()
		}
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Closing the listener stops new control/spawn requests before engine
		// cleanup begins. Reset cancels/drains subagents, permissions and ACP
		// children; Shutdown then drains any remaining HTTP handlers.
		_ = listener.Close()
		if cleanup != nil {
			cleanup()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		err := <-serveErr
		if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
			return shutdownErr
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			return err
		}
		return nil
	}
}

func currentExecutablePath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return ""
	}
	return absolute
}

func resolveMocksDir(flagValue, envValue, cwd, executable string) string {
	configured := strings.TrimSpace(flagValue)
	if configured == "" {
		configured = strings.TrimSpace(envValue)
	}
	if configured != "" {
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(cwd, configured)
		}
		return filepath.Clean(configured)
	}
	if strings.TrimSpace(executable) == "" {
		return ""
	}
	candidate := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "desktop", "docs", "mocks"))
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return ""
	}
	return candidate
}

// daemonEventBroadcaster makes the generation Store the visibility boundary for
// every job event. Non-terminal journal writes finish outside the session mutex
// before delivery; terminal canonical/archive persistence continues without the
// global dispatch lock so unrelated chats remain able to publish and stream.
func daemonEventBroadcaster(sessionState *sessionStore, broadcast func(string, any)) func(string, any) {
	var dispatchMu sync.Mutex
	return func(channel string, payload any) {
		// Manager timers/provider callbacks may emit concurrently. Keep queue
		// admission and visible delivery in one order so the recovery mirror can
		// never persist A→B while clients observed B→A.
		dispatchMu.Lock()
		if channel != "job:event" {
			if channel == "agent:apply" {
				persistAgentApplyControls(sessionState, payload)
			}
			if broadcast != nil {
				broadcast(channel, payload)
			}
			dispatchMu.Unlock()
			return
		}
		published := make(chan struct{})
		done := make(chan struct{})
		go func() {
			sessionState.recordJobEvent(channel, payload, func() { close(published) })
			close(done)
		}()
		<-published
		terminal := fieldString(mapFromAnyMain(payload), "type") == "end"
		if !terminal {
			if broadcast != nil {
				broadcast(channel, payload)
			}
			dispatchMu.Unlock()
			return
		}
		dispatchMu.Unlock()
		<-done
		if broadcast != nil {
			broadcast(channel, payload)
		}
	}
}

func persistAgentApplyControls(sessionState *sessionStore, payload any) {
	if sessionState == nil {
		return
	}
	item := mapFromAnyMain(payload)
	if fieldString(item, "action") != "session-refresh" {
		return
	}
	tabID, chatID := fieldString(item, "tabId"), fieldString(item, "chatId")
	if tabID == "" || chatID == "" {
		return
	}
	sessionState.UpdateChatControls(
		tabID, chatID, fieldString(item, "providerId"),
		firstNonEmptyString(fieldString(item, "modelId"), fieldString(item, "currentModelId")),
		firstNonEmptyString(fieldString(item, "modeId"), fieldString(item, "currentModeId")),
	)
}

// hydratableStoredModelID filters durable ids that are placeholders, not real
// selections: "default" is the renderer's no-explicit-choice sentinel and can
// never be applied to an adapter (attempting to would fail session startup).
func hydratableStoredModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if strings.EqualFold(modelID, "default") {
		return ""
	}
	return modelID
}

func applyStoredChatRuntimeControls(sessionState *sessionStore, arg map[string]any) {
	if sessionState == nil || arg == nil {
		return
	}
	providerID, modelID, modeID, ok := sessionState.ChatRuntimeControls(fieldString(arg, "tabId"), fieldString(arg, "chatId"))
	if !ok {
		return
	}
	modelID = hydratableStoredModelID(modelID)
	if fieldString(arg, "providerId") == "" && providerID != "" {
		arg["providerId"] = providerID
	}
	if firstNonEmptyString(fieldString(arg, "modelId"), fieldString(arg, "currentModelId"), fieldString(arg, "model")) == "" && modelID != "" {
		arg["modelId"] = modelID
	}
	if firstNonEmptyString(fieldString(arg, "modeId"), fieldString(arg, "currentModeId"), fieldString(arg, "mode")) == "" && modeID != "" {
		arg["modeId"] = modeID
	}
}

func prodModeDefault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WORKASS_PROD"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func applyProductionDefaults(goos string, prod bool, flagOverrides map[string]bool, port *int, bind *string) {
	if !prod || goos != "windows" {
		return
	}
	if !flagOverrides["port"] {
		*port = 80
	}
	if !flagOverrides["bind"] {
		*bind = "lan"
	}
}

type stubDef struct {
	channel string
	result  func() any
}

func registerDaemonHandlers(hub *wire.Hub, cwd string, acpManager *acp.Manager, options ...daemonOptions) int {
	var opts daemonOptions
	if len(options) > 0 {
		opts = options[0]
	}
	state := newDaemonState(cwd, opts)
	if acpManager != nil {
		acpManager.SetModelScores(state.setting.get()["modelScores"])
	}
	count := 0
	hub.Register("app:meta", func(args []any) (any, error) {
		return appMeta(cwd), nil
	})
	count++
	hub.Register("state:get", func(args []any) (any, error) {
		return readState(cwd), nil
	})
	count++
	hub.Register("fs:list-dir", func(args []any) (any, error) {
		var requested string
		if len(args) > 0 && args[0] != nil {
			requested = strings.TrimSpace(fmt.Sprint(args[0]))
		}
		return listServerDirectories(cwd, requested), nil
	})
	count++

	for _, def := range daemonStubs() {
		registerStub(hub, def.channel, def.result)
		count++
	}
	sessionState := sharedSessionStore(state.stateDir)
	registerSessionHandlers(hub, sessionState, acpManager)
	count += 2
	registerStateDigestHandler(hub, sessionState, acpManager, state.setting)
	count++
	registerArchiveHandlers(hub, state)
	count += 2
	registerNotifyHandlers(hub)
	count++
	registerConfigSettingsHandlers(hub, state, acpManager)
	count += 4
	registerVoiceHandlers(hub)
	count += 2
	if acpManager != nil {
		chatControl := opts.ChatControl
		if chatControl == nil {
			chatControl = newChatControlCoordinator(acpManager, sessionState, hub.Broadcast)
		}
		registerAcpHandlers(hub, acpManager, state.stateDir, sessionState, chatControl)
		count += 23
	}
	return count
}

func registerStateDigestHandler(hub *wire.Hub, store *sessionStore, manager *acp.Manager, settings *appSettingsStore) {
	hub.Register("state:digest", func(args []any) (any, error) {
		catalogHashes := map[string]string{}
		processes := any([]map[string]any{})
		if manager != nil {
			for _, group := range manager.CatalogSnapshotGroups() {
				catalogHashes[group.ProviderID] = stateDigestHash(group)
			}
			processes = manager.Processes()
		}
		settingsValue := any(map[string]any{})
		if settings != nil {
			settingsValue = settings.get()
		}
		return store.StateDigest(manager, catalogHashes, stateDigestHash(settingsValue), stateDigestHash(processes)), nil
	})
}

func stateDigestHash(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte("null")
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func registerSessionHandlers(hub *wire.Hub, store *sessionStore, managers ...*acp.Manager) {
	var manager *acp.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	hub.Register("session:get", func(args []any) (any, error) {
		raw := store.GetRawWithLiveSessions(manager)
		if raw == nil {
			return nil, nil
		}
		return wire.RawResult(raw), nil
	})
	hub.Register("session:save", func(args []any) (any, error) {
		var snapshot any = map[string]any{}
		if len(args) > 0 && args[0] != nil {
			snapshot = args[0]
		}
		saved := store.Save(snapshot)
		if saved && manager != nil {
			// ReconcileNativeSessionOwners only reads chat id/chatId pairs. Passing
			// store.Get() cloned the whole multi-megabyte mirror a second time on
			// every save, inside the invoke that also gates later renderer frames.
			manager.ReconcileNativeSessionOwners(store.ChatIdentitySnapshot())
		}
		return saved, nil
	})
}

func registerStub(hub *wire.Hub, channel string, result func() any) {
	hub.Register(channel, func(args []any) (any, error) {
		// TODO(P2): real implementation.
		return result(), nil
	})
}

func registerNotifyHandlers(hub *wire.Hub) {
	hub.Register("notify", func(args []any) (any, error) {
		arg := firstMapArg(args)
		hub.Broadcast("notify", notifyPayload(fieldString(arg, "title"), fieldString(arg, "body"), fieldString(arg, "tabId")))
		return true, nil
	})
}

// registerVoiceHandlers exposes dictation: one status probe and one transcribe.
//
// Two channels rather than one because "whisper is not installed" is a setup
// problem with a one-line fix, and a microphone button that fails at the end of
// a sentence is the worst possible place to discover it. The client asks first
// and can say so before the user speaks.
//
// Additive channels only — the wire contract is frozen, and the renderer that
// never calls these keeps working exactly as it did.
func registerVoiceHandlers(hub *wire.Hub) {
	hub.Register("voice:status", func(args []any) (any, error) {
		engine, err := voice.Locate()
		payload := map[string]any{"available": err == nil}
		switch {
		case errors.Is(err, voice.ErrEngineMissing):
			payload["reason"] = "engine-missing"
			payload["hint"] = "brew install whisper-cpp"
		case errors.Is(err, voice.ErrModelMissing):
			payload["reason"] = "model-missing"
			// The .en weights are deliberately not suggested: they cannot
			// handle a sentence that switches language halfway.
			payload["hint"] = "download a multilingual ggml model into ~/.local/share/whisper-models"
		case err != nil:
			payload["reason"] = "unavailable"
			payload["hint"] = err.Error()
		default:
			payload["model"] = filepath.Base(engine.Model)
		}
		return payload, nil
	})

	hub.Register("voice:transcribe", func(args []any) (any, error) {
		arg := firstMapArg(args)
		raw := fieldString(arg, "audio")
		if raw == "" {
			return nil, errors.New("voice:transcribe needs base64 audio")
		}
		wav, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("voice:transcribe: audio is not base64: %w", err)
		}
		engine, err := voice.Locate()
		if err != nil {
			return nil, err
		}
		res, err := engine.Transcribe(context.Background(), voice.Request{
			WAV:   wav,
			Lang:  fieldString(arg, "lang"),
			Vocab: stringsField(arg, "vocab"),
		})
		if err != nil {
			return nil, err
		}
		// Text only. The recording is not returned, not broadcast and not kept:
		// one caller asked for words and gets words.
		return map[string]any{
			"text":  res.Text,
			"model": res.Model,
			"ms":    res.Duration.Milliseconds(),
		}, nil
	})
}

// stringsField reads a JSON array of strings, tolerating the single-string form
// a hand-written client is likely to send.
func stringsField(m map[string]any, key string) []string {
	if m == nil || m[key] == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if s := strings.TrimSpace(v); s != "" {
			return []string{s}
		}
	}
	return nil
}

func registerConfigSettingsHandlers(hub *wire.Hub, state *daemonState, manager *acp.Manager) {
	hub.Register("settings:get", func(args []any) (any, error) {
		var models any = []any{}
		var modes any = []any{}
		if manager != nil {
			catalog := manager.Catalog(context.Background())
			models = catalog["models"]
			modes = catalog["modes"]
		}
		return map[string]any{
			"settings":  state.setting.get(),
			"models":    models,
			"modes":     modes,
			"taskKinds": taskKinds(),
		}, nil
	})
	hub.Register("settings:set", func(args []any) (any, error) {
		saved, err := state.setting.set(firstMapArg(args))
		if err != nil {
			return nil, err
		}
		if manager != nil {
			manager.SetModelScores(saved["modelScores"])
		}
		hub.Broadcast("settings:changed", saved)
		return saved, nil
	})
	hub.Register("config:get", func(args []any) (any, error) {
		return state.config.getRedacted(), nil
	})
	hub.Register("config:set", func(args []any) (any, error) {
		saved, err := state.config.set(firstMapArg(args))
		if err != nil {
			return nil, err
		}
		hub.Broadcast("config:changed", saved)
		return saved, nil
	})
}

func registerAcpHandlers(hub *wire.Hub, manager *acp.Manager, stateDir string, sessionState *sessionStore, chatControl *chatControlCoordinator) {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = manager.StateDir()
	}
	hub.SetOnClientReady(func(send func(channel string, payload any) error) {
		manager.ReplayProviderEvents(send)
		_ = send("agent:apply", map[string]any{"action": "session-refresh"})
	})
	hub.SetOnControllerReady(func(send func(channel string, payload any) error) {
		manager.ReplayPendingPermissions(send)
	})
	hub.Register("app-chat:new-session", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if refresh, _ := boolField(arg, "refreshPlanUsage"); refresh {
			return manager.RefreshPlanUsageSession(context.Background(), parseSessionOptions(arg))
		}
		oldSessionID := fieldString(arg, "replaceSessionId")
		workspaceRebind, _ := boolField(arg, "workspaceRebind")
		if oldSessionID != "" || workspaceRebind {
			expectedRevision, ok := intFieldPresent(arg, "expectedWorkspaceRevision")
			if !ok || expectedRevision < 0 || sessionState == nil {
				return map[string]any{"error": "workspace rebind requires daemon revision authority", "workspaceCommitted": false}, nil
			}
			workspaceRevision := expectedRevision
			committed, err := manager.InvalidateChatWorkspace(context.Background(), oldSessionID, parseSessionOptions(arg), func() error {
				next, moved := sessionState.MoveChatWorkspace(
					fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "cwd"), expectedRevision,
				)
				if moved {
					workspaceRevision = next
					return nil
				}
				return errors.New("workspace changed in another controller; reload before moving")
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "workspaceCommitted": committed}, nil
			}
			return map[string]any{
				"sessionId": "", "cwd": fieldString(arg, "cwd"), "models": []any{}, "modes": []any{},
				"workspaceCommitted": true, "workspaceRebound": true, "workspaceRevision": workspaceRevision,
			}, nil
		}
		// Once a workspace move has committed, the daemon snapshot owns cwd. A
		// stale/reconnected controller may still ask to create the replacement
		// session using its pre-move cwd; never let that recreate the provider
		// thread in the old directory.
		if sessionState != nil {
			if cwd, ok := sessionState.ChatWorkspace(fieldString(arg, "tabId"), fieldString(arg, "chatId")); ok {
				arg["cwd"] = cwd
			}
			applyStoredChatRuntimeControls(sessionState, arg)
		}
		info, err := manager.NewSession(context.Background(), parseSessionOptions(arg))
		if err != nil {
			return map[string]any{"error": err.Error(), "models": []any{}, "modes": []any{}}, nil
		}
		if sessionState != nil {
			sessionState.UpdateChatControls(
				fieldString(arg, "tabId"), fieldString(arg, "chatId"), info.ProviderID,
				stringPointerValue(info.CurrentModelID), stringPointerValue(info.CurrentModeID),
			)
		}
		return info, nil
	})
	hub.Register("app-chat:refresh-plan-usage", func(args []any) (any, error) {
		providerID := strings.ToLower(strings.TrimSpace(fieldString(firstMapArg(args), "providerId")))
		if err := manager.RefreshProviderPlanUsage(context.Background(), providerID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "providerId": providerID}, nil
	})
	hub.Register("app-chat:fork", func(args []any) (any, error) {
		arg := firstMapArg(args)
		sourceTabID := fieldString(arg, "tabId")
		newTabID := fieldString(arg, "newTabId")
		if sourceTabID == "" || newTabID == "" {
			return nil, fmt.Errorf("app-chat:fork requires {tabId, newTabId}")
		}
		if sourceTabID == newTabID {
			return nil, fmt.Errorf("app-chat:fork requires a distinct newTabId")
		}
		atTurn, hasAtTurn := intFieldPresent(arg, "atTurn")
		info, err := manager.ForkSession(context.Background(), acp.ForkOptions{
			TabID:     sourceTabID,
			NewTabID:  newTabID,
			ChatID:    fieldString(arg, "chatId"),
			NewChatID: firstNonEmptyString(fieldString(arg, "newChatId"), fieldString(arg, "chatIdNew")),
			CWD:       fieldString(arg, "cwd"),
		})
		if err != nil {
			return map[string]any{"error": err.Error(), "models": []any{}, "modes": []any{}}, nil
		}
		effectiveTurn, err := copyChatArchivePrefix(stateDir, sourceTabID, newTabID, atTurn, hasAtTurn)
		if err != nil {
			manager.CloseSessionAndForget(context.Background(), info.SessionID)
			return nil, err
		}
		if sessionState != nil {
			sessionState.UpdateChatControls(
				newTabID, firstNonEmptyString(fieldString(arg, "newChatId"), fieldString(arg, "chatIdNew")), info.ProviderID,
				stringPointerValue(info.CurrentModelID), stringPointerValue(info.CurrentModeID),
			)
		}
		return sessionInfoWithFork(info, sourceTabID, effectiveTurn), nil
	})
	hub.Register("app-chat:close-session", func(args []any) (any, error) {
		return manager.CloseSessionAndForget(context.Background(), stringArg(args, 0)), nil
	})
	hub.Register("app-chat:reset", func(args []any) (any, error) {
		return manager.Reset(), nil
	})
	hub.Register("app-chat:set-model", func(args []any) (any, error) {
		arg := firstMapArg(args)
		sessionID := fieldString(arg, "sessionId")
		modelID := fieldString(arg, "modelId")
		result, err := manager.SetModel(context.Background(), sessionID, modelID)
		if err == nil && sessionState != nil {
			if binding, ok := manager.LiveSession(sessionID); ok {
				persistedModelID := strings.TrimSpace(fieldString(result, "currentModelId"))
				if persistedModelID == "" {
					persistedModelID = modelID
				}
				sessionState.UpdateChatControls(binding.TabID, binding.ChatID, binding.Info.ProviderID, persistedModelID, "")
			}
		}
		return result, err
	})
	hub.Register("app-chat:set-mode", func(args []any) (any, error) {
		arg := firstMapArg(args)
		sessionID := fieldString(arg, "sessionId")
		modeID := fieldString(arg, "modeId")
		result, err := manager.SetMode(context.Background(), sessionID, modeID)
		if err == nil && sessionState != nil {
			if binding, ok := manager.LiveSession(sessionID); ok {
				sessionState.UpdateChatControls(binding.TabID, binding.ChatID, binding.Info.ProviderID, "", modeID)
			}
		}
		return result, err
	})
	hub.Register("app-chat:steer", func(args []any) (any, error) {
		arg := firstMapArg(args)
		sessionID := fieldString(arg, "sessionId")
		prompt := fieldString(arg, "prompt")
		clientUserMessageID := fieldString(arg, "clientUserMessageId")
		continuationAssistantMessageID := fieldString(arg, "continuationAssistantMessageId")
		boundary := mapFromAnyMain(arg["boundary"])
		binding, bound := manager.LiveSession(sessionID)
		if sessionState != nil && clientUserMessageID != "" && bound && binding.ChatID != "" {
			if err := sessionState.BeginLiveSteer(binding.TabID, binding.ChatID, clientUserMessageID, prompt, continuationAssistantMessageID, sliceArg(arg["images"]), boundary); err != nil {
				return map[string]any{
					"ok": false, "queued": true, "strategy": "queue",
					"error": "steer could not be written into chronological history",
				}, nil
			}
		}
		result := manager.Steer(sessionID, prompt, sliceArg(arg["images"]), clientUserMessageID)
		strategy := fieldString(result, "strategy")
		if sessionState != nil && clientUserMessageID != "" && (result["live"] == true || strategy == "uncertain") {
			if bound && binding.ChatID != "" {
				outcome := "accepted"
				if strategy == "uncertain" {
					outcome = "uncertain"
				}
				if err := sessionState.AcknowledgeLiveSteer(binding.TabID, binding.ChatID, clientUserMessageID, prompt, outcome); err != nil {
					// Native delivery already happened (or is uncertain). Never turn a
					// persistence failure into an explicit rejection that the renderer
					// would replay through FIFO and potentially duplicate.
					result["persistenceError"] = "steer acknowledgement could not be persisted"
				}
				deferred, _ := boundary["deferUntilConsumed"].(bool)
				receipt, _ := result["receipt"].(bool)
				if outcome == "accepted" && deferred && !receipt {
					if err := sessionState.CommitLiveSteerBoundary(binding.TabID, binding.ChatID, clientUserMessageID); err != nil {
						result["persistenceError"] = "steer boundary could not be persisted"
					}
				}
			}
		} else if sessionState != nil && clientUserMessageID != "" && bound && binding.ChatID != "" {
			if err := sessionState.RejectLiveSteer(binding.TabID, binding.ChatID, clientUserMessageID); err != nil {
				// The native provider rejected the input, but the durable transcript
				// could not safely transfer ownership to FIFO. Keep one unconfirmed
				// row rather than risking a duplicate replay.
				result["strategy"] = "uncertain"
				result["queued"] = false
				result["persistenceError"] = "steer rejection could not be reconciled"
			}
		}
		return result, nil
	})
	hub.Register("app-chat:use-rate-limit-reset", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return manager.ConsumeRateLimitResetCredit(
			context.Background(),
			fieldString(arg, "providerId"),
			fieldString(arg, "sessionId"),
			fieldString(arg, "idempotencyKey"),
			fieldString(arg, "creditId"),
		)
	})
	// Read-only: the per-chat provider catalog (commands/agents/output styles)
	// for late clients — the chat:commands event covers the live ones.
	hub.Register("chat:commands-get", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return manager.ChatCommands(fieldString(arg, "tabId"), fieldString(arg, "chatId")), nil
	})
	hub.Register("spawned-work:list", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
		if tabID == "" || chatID == "" {
			return nil, errors.New("spawned-work:list requires {tabId, chatId}")
		}
		reply := map[string]any{"items": manager.ListSpawnedWork(tabID, chatID)}
		// The obligation rides the same reply for the same reason it rides
		// spawned-work:changed: it is derived from this exact state. Without it
		// a freshly attached client (renderer reload, a phone joining) would
		// only learn what the chat owes on the next background change, which
		// for a quiet chat never comes.
		if obligation := manager.ObligationFor(tabID, chatID); obligation != nil {
			reply["obligation"] = obligation
		}
		return reply, nil
	})
	hub.Register("spawned-work:read", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID, id := fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "id")
		if tabID == "" || chatID == "" || id == "" {
			return nil, errors.New("spawned-work:read requires {tabId, chatId, id}")
		}
		return manager.ReadSpawnedWork(tabID, chatID, id, intField(arg, "tailBytes")), nil
	})
	// The stop square. Mutating, so the wire already requires the controller
	// lease before this handler is reached — the same bar proc:kill sits behind.
	hub.Register("spawned-work:stop", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID, id := fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "id")
		if tabID == "" || chatID == "" || id == "" {
			return nil, errors.New("spawned-work:stop requires {tabId, chatId, id}")
		}
		return manager.StopSpawnedWork(tabID, chatID, id), nil
	})
	hub.Register("job:start", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if sessionState != nil {
			// Workspace cwd is daemon-authoritative after a transactional sidebar
			// move. A stale controller may still submit the previous value, but it
			// can never make the next job execute there.
			if cwd, ok := sessionState.ChatWorkspace(fieldString(arg, "tabId"), fieldString(arg, "chatId")); ok {
				arg["cwd"] = cwd
			}
			applyStoredChatRuntimeControls(sessionState, arg)
		}
		jobOpts := parseJobStartOptions(arg)
		// job:start is the controller submitting what the user typed, so this
		// path is human-authored by construction.
		jobOpts.HumanAuthored = true
		prepared := false
		if sessionState != nil {
			jobOpts.BeforeStart = func(target *acp.JobStartOptions) error {
				if !sessionState.PrepareTurn(arg) && jobOpts.QueueID != "" {
					if _, adopted := sessionState.AdoptedQueueReceipt(fieldString(arg, "tabId"), fieldString(arg, "chatId"), jobOpts.QueueID); adopted {
						return errQueueRowAdopted
					}
				}
				if fields, ok := sessionState.PreparedTurnPublicFields(fieldString(arg, "tabId")); ok {
					target.PromptText = fields["promptText"]
					target.UserMessageID = fields["userMessageId"]
					target.AssistantMessageID = fields["assistantMessageId"]
				}
				target.HumanAuthored = true
				prepared = true
				return nil
			}
		}
		job, err := manager.StartJob(context.Background(), jobOpts)
		if err != nil && errors.Is(err, errQueueRowAdopted) && sessionState != nil {
			return sessionState.QueueRendererStartCollision(arg)
		}
		if err != nil && errors.Is(err, acp.ErrChatBusy) && fieldString(arg, "busyMode") == "queue-v1" && sessionState != nil {
			receipt, queueErr := sessionState.QueueRendererStartCollision(arg)
			if queueErr != nil {
				return nil, queueErr
			}
			if chatControl != nil {
				tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
				chatControl.refresh(tabID, chatID, false)
				chatControl.scheduleDrain(tabID, chatID)
			}
			return receipt, nil
		}
		if err != nil && sessionState != nil {
			if !prepared {
				sessionState.PrepareTurn(arg)
			}
			sessionState.FailPreparedTurn(fieldString(arg, "tabId"), "Error: "+err.Error())
		}
		return job, err
	})
	hub.Register("job:cancel", func(args []any) (any, error) {
		return manager.CancelJobResult(stringArg(args, 0)), nil
	})
	hub.Register("chat:permission-decide", func(args []any) (any, error) {
		arg := firstMapArg(args)
		id := fieldString(arg, "id")
		if id == "" {
			return map[string]any{"ok": false}, nil
		}
		return map[string]any{"ok": manager.PermissionDecide(id, fieldString(arg, "optionId"))}, nil
	})
	hub.Register("chat:permissions-pending", func(args []any) (any, error) {
		return map[string]any{"permissions": manager.PendingPermissions()}, nil
	})
	hub.Register("chat:env-get", func(args []any) (any, error) {
		arg := firstMapArg(args)
		chatID := fieldString(arg, "chatId")
		tabID := fieldString(arg, "tabId")
		if chatID == "" && tabID == "" {
			chatID = stringArg(args, 0)
		}
		return manager.ChatEnvGet(chatID, tabID), nil
	})
	hub.Register("chat:checkpoints", func(args []any) (any, error) {
		arg := firstMapArg(args)
		chatID := fieldString(arg, "chatId")
		tabID := fieldString(arg, "tabId")
		if chatID == "" && tabID == "" {
			chatID = stringArg(args, 0)
		}
		return manager.ChatCheckpoints(chatID, tabID), nil
	})
	hub.Register("chat:rewind", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return manager.ChatRewind(context.Background(), fieldString(arg, "chatId"), intField(arg, "turnSeq"))
	})
	hub.Register("chat:diff", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return manager.ChatDiff(context.Background(), fieldString(arg, "chatId"), fieldString(arg, "repo"), fieldString(arg, "path"))
	})
	hub.Register("providers:list", func(args []any) (any, error) {
		return manager.ProvidersList(), nil
	})
	hub.Register("providers:detect", func(args []any) (any, error) {
		return manager.DetectProviders(context.Background(), parseDetectOptions(firstMapArg(args))), nil
	})
	hub.Register("app-chat:detect-acp", func(args []any) (any, error) {
		return manager.DetectProviders(context.Background(), parseDetectOptions(firstMapArg(args))), nil
	})
	hub.Register("providers:update", func(args []any) (any, error) {
		arg := firstMapArg(args)
		providerID := fieldString(arg, "providerId")
		if providerID == "" {
			providerID = fieldString(arg, "id")
		}
		if providerID == "" {
			providerID = stringArg(args, 0)
		}
		return manager.StartProviderUpdate(context.Background(), providerID)
	})
	hub.Register("providers:toggle", func(args []any) (any, error) {
		arg := firstMapArg(args)
		id := fieldString(arg, "id")
		enabled, ok := boolField(arg, "enabled")
		if id == "" || !ok {
			return nil, fmt.Errorf("providers:toggle requires {id, enabled}")
		}
		return manager.ToggleProvider(context.Background(), id, enabled)
	})
	hub.Register("proc:list", func(args []any) (any, error) {
		return map[string]any{"processes": manager.Processes()}, nil
	})
	hub.Register("proc:read", func(args []any) (any, error) {
		return manager.ReadProcess(stringArg(args, 0)), nil
	})
	hub.Register("proc:kill", func(args []any) (any, error) {
		arg := firstMapArg(args)
		id := fieldString(arg, "id")
		if id == "" {
			id = stringArg(args, 0)
		}
		return manager.KillProcess(id), nil
	})
	hub.Register("proc:kill-all", func(args []any) (any, error) {
		return manager.KillAllProcesses(), nil
	})
}

func parseStringArrayFlag(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	var values []any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&values); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprint(value))
	}
	return out, nil
}

func replaceProviderConfig(providers []acp.ProviderConfig, provider acp.ProviderConfig) []acp.ProviderConfig {
	for i := range providers {
		if strings.EqualFold(strings.TrimSpace(providers[i].ID), strings.TrimSpace(provider.ID)) {
			providers[i] = provider
			return providers
		}
	}
	return append(providers, provider)
}

func parseDetectOptions(arg map[string]any) acp.DetectOptions {
	return acp.DetectOptions{ProviderID: fieldString(arg, "provider")}
}

func firstMapArg(args []any) map[string]any {
	if len(args) == 0 {
		return map[string]any{}
	}
	if m, ok := args[0].(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

func stringArg(args []any, idx int) string {
	if idx < 0 || idx >= len(args) {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(args[idx]))
}

func fieldString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok && v != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

// boolFieldValue drops the presence flag for callers where an absent field and
// an explicit false mean the same thing.
func boolFieldValue(m map[string]any, key string) bool {
	value, _ := boolField(m, key)
	return value
}

func boolField(m map[string]any, key string) (bool, bool) {
	if m == nil || m[key] == nil {
		return false, false
	}
	switch v := m[key].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

func parseSessionOptions(m map[string]any) acp.SessionOptions {
	return acp.SessionOptions{
		CWD:        fieldString(m, "cwd"),
		BridgeKey:  fieldString(m, "bridgeKey"),
		TabID:      firstNonEmptyString(fieldString(m, "tabId"), fieldString(m, "chatTabId")),
		ChatID:     fieldString(m, "chatId"),
		SessionID:  fieldString(m, "sessionId"),
		ProviderID: fieldString(m, "providerId"),
		ModelID:    firstNonEmptyString(fieldString(m, "modelId"), fieldString(m, "currentModelId"), fieldString(m, "model")),
		ModeID:     firstNonEmptyString(fieldString(m, "modeId"), fieldString(m, "currentModeId"), fieldString(m, "mode")),
	}
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func parseJobStartOptions(m map[string]any) acp.JobStartOptions {
	return acp.JobStartOptions{
		Kind:               fieldString(m, "kind"),
		Key:                m["key"],
		Title:              fieldString(m, "title"),
		PermissionMode:     fieldString(m, "permissionMode"),
		ChatID:             fieldString(m, "chatId"),
		TabID:              fieldString(m, "tabId"),
		SessionID:          fieldString(m, "sessionId"),
		CWD:                fieldString(m, "cwd"),
		Prompt:             fieldString(m, "prompt"),
		Message:            fieldString(m, "message"),
		History:            sliceArg(m["history"]),
		HistoryCharBudget:  firstNonZeroInt(intField(m, "historyCharBudget"), intField(m, "historyBudgetChars")),
		ContextSize:        firstNonZeroInt(intField(m, "contextSize"), intField(m, "contextWindow")),
		Images:             sliceArg(m["images"]),
		ModelID:            firstNonEmptyString(fieldString(m, "modelId"), fieldString(m, "model")),
		ModeID:             firstNonEmptyString(fieldString(m, "modeId"), fieldString(m, "mode")),
		ProviderID:         fieldString(m, "providerId"),
		HumanAuthored:      boolFieldValue(m, "humanAuthored"),
		UserMessageID:      fieldString(m, "userMessageId"),
		AssistantMessageID: fieldString(m, "assistantMessageId"),
		QueueID:            firstNonEmptyString(fieldString(m, "queueId"), fieldString(m, agentQueueMessageField)),
		PromptText:         fieldString(m, "promptText"),
	}
}

func intField(m map[string]any, key string) int {
	if m == nil || m[key] == nil {
		return 0
	}
	switch v := m[key].(type) {
	case json.Number:
		n, _ := strconv.Atoi(v.String())
		return n
	case float64:
		return int(v)
	case int:
		return v
	default:
		n, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return n
	}
}

func intFieldPresent(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	if _, ok := m[key]; !ok || m[key] == nil {
		return 0, false
	}
	return intField(m, key), true
}

func firstNonZeroInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func sliceArg(v any) []any {
	items, _ := v.([]any)
	if items == nil {
		return nil
	}
	return items
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sessionInfoWithFork(info acp.SessionInfo, sourceTabID string, atTurn int) map[string]any {
	data, _ := json.Marshal(info)
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&out)
	if out == nil {
		out = map[string]any{}
	}
	out["forkedFrom"] = map[string]any{"tabId": sourceTabID, "atTurn": atTurn}
	return out
}

func notifyPayload(title, body, tabID string) map[string]any {
	title = acp.RedactSensitiveText(strings.TrimSpace(title))
	body = acp.RedactSensitiveText(strings.TrimSpace(body))
	var tab any
	if strings.TrimSpace(tabID) != "" {
		tab = strings.TrimSpace(tabID)
	}
	return map[string]any{
		"title": title,
		"body":  body,
		"tabId": tab,
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func daemonStubs() []stubDef {
	return []stubDef{
		{"lan:access-decide", func() any { return map[string]any{"ok": false} }},
		{"jira:get", func() any { return map[string]any{"syncedAt": nil, "issues": []any{}} }},
		{"activity:get", func() any { return map[string]any{"jobs": []any{}, "logs": map[string]any{}} }},
		{"activity:clear", func() any { return false }},
		{"deploy:catalog", func() any { return map[string]any{"apis": []any{}} }},
		{"deploy:versions", func() any {
			return map[string]any{
				"ok":                    false,
				"host":                  "",
				"repo":                  "",
				"branch":                "",
				"localRepo":             nil,
				"versions":              []any{},
				"selected":              "",
				"source":                "",
				"error":                 "",
				"requiresManualVersion": false,
				"deploymentEnvironment": "",
				"checkedAt":             "",
				"cached":                false,
			}
		}},
		{"skills:list", func() any { return []any{} }},
		{"teams:refresh", func() any { return map[string]any{"ok": false, "code": nil} }},
		{"jira:sync", func() any { return map[string]any{"ok": false, "code": nil} }},
		{"deploy:preflight", func() any {
			return map[string]any{
				"checkedAt": "",
				"env":       nil,
				"cluster":   "",
				"githubOrg": "",
				"sources":   []any{},
			}
		}},
		{"deploy:auth", emptyPublicJob},
		{"job:start", func() any { return nil }},
		{"chat:kill-terminal", func() any { return map[string]any{"ok": false, "error": ""} }},
		{"chat:kill-command", func() any {
			return map[string]any{"ok": false, "killed": []any{}, "matched": 0, "error": "", "unsupported": true}
		}},
		{"app-chat:steer", func() any { return map[string]any{"ok": false, "unsupported": true, "error": ""} }},
		{"app-chat:use-rate-limit-reset", func() any { return map[string]any{"outcome": "noCredit"} }},
		{"spawned-work:list", func() any { return map[string]any{"items": []any{}} }},
		{"spawned-work:read", func() any { return map[string]any{"ok": false, "error": "ACP unavailable"} }},
		{"spawned-work:stop", func() any { return map[string]any{"ok": false, "error": "ACP unavailable"} }},
		{"job:cancel", func() any { return acp.JobCancelResult{Reason: "unknown"} }},
		{"proc:list", func() any { return map[string]any{"processes": []any{}} }},
		{"proc:read", func() any { return map[string]any{"ok": false, "error": ""} }},
		{"proc:kill", func() any { return map[string]any{"ok": false, "error": ""} }},
		{"agent-proc:list", func() any { return map[string]any{"ok": true, "unsupported": true, "processes": []any{}} }},
		{"agent-proc:kill", func() any {
			return map[string]any{"ok": false, "killed": []any{}, "error": "", "unsupported": true}
		}},
		{"proc:kill-all", func() any { return map[string]any{"ok": true, "stopped": 0} }},
		{"app-chat:reset", func() any { return true }},
		{"app-chat:detect-acp", func() any { return map[string]any{"ok": true, "detected": []any{}, "results": []any{}} }},
		{"app-chat:new-session", func() any { return map[string]any{"error": "", "models": []any{}, "modes": []any{}} }},
		{"code:unlock", func() any { return map[string]any{"ok": false} }},
		{"code:lock", func() any { return map[string]any{"ok": true} }},
		{"code:tree", func() any { return map[string]any{"ok": false, "error": "locked"} }},
		{"code:read", func() any { return map[string]any{"ok": false, "error": "locked"} }},
		{"app-chat:close-session", func() any { return false }},
		{"app-chat:set-model", func() any { return map[string]any{"currentModelId": nil} }},
		{"app-chat:set-mode", func() any { return map[string]any{"currentModeId": nil} }},
		{"chat:permission-decide", func() any { return map[string]any{"ok": false} }},
		{"chat:permissions-pending", func() any { return map[string]any{"permissions": []any{}} }},
		{"draft:save", func() any { return map[string]any{"key": nil, "text": nil, "updatedAt": ""} }},
		{"status:set", func() any { return map[string]any{"key": nil, "status": "open", "updatedAt": ""} }},
		{"teams:share-link", func() any { return map[string]any{"ok": false, "error": "", "setup": true} }},
	}
}

// listServerDirectories implements the frozen fs:list-dir channel on the
// daemon host. Clients receive directory metadata only; they never browse the
// filesystem of the Electron/LAN viewing device.
func listServerDirectories(rootDir, requested string) map[string]any {
	type directoryEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	empty := func(path any, parent any, entries []directoryEntry, err error) map[string]any {
		result := map[string]any{"path": path, "parent": parent, "entries": entries}
		if err != nil {
			result["error"] = acp.RedactSensitiveText(err.Error())
		}
		return result
	}
	if strings.TrimSpace(requested) == "" {
		entries := make([]directoryEntry, 0, 30)
		seen := map[string]struct{}{}
		add := func(name, candidate string) {
			if strings.TrimSpace(candidate) == "" {
				return
			}
			abs, err := filepath.Abs(candidate)
			if err != nil {
				return
			}
			abs = filepath.Clean(abs)
			info, err := os.Stat(abs)
			if err != nil || !info.IsDir() {
				return
			}
			key := abs
			if runtime.GOOS == "windows" {
				key = strings.ToLower(abs)
			}
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
			entries = append(entries, directoryEntry{Name: name, Path: abs})
		}
		if home, err := os.UserHomeDir(); err == nil {
			add("Inicio", home)
		}
		add("Workspace", workspaceDirectory(rootDir))
		add("App", rootDir)
		if runtime.GOOS == "windows" {
			for letter := 'C'; letter <= 'Z'; letter++ {
				root := fmt.Sprintf("%c:%c", letter, os.PathSeparator)
				add(root, root)
			}
		}
		return empty(nil, nil, entries, nil)
	}

	abs, err := filepath.Abs(requested)
	if err != nil {
		return empty(requested, nil, []directoryEntry{}, err)
	}
	abs = filepath.Clean(abs)
	items, err := os.ReadDir(abs)
	if err != nil {
		return empty(abs, nil, []directoryEntry{}, err)
	}
	entries := make([]directoryEntry, 0, len(items))
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		entries = append(entries, directoryEntry{Name: item.Name(), Path: filepath.Join(abs, item.Name())})
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if left == right {
			return entries[i].Name < entries[j].Name
		}
		return left < right
	})
	parent := any(nil)
	if candidate := filepath.Dir(abs); candidate != abs {
		parent = candidate
	}
	return empty(abs, parent, entries, nil)
}

func appMeta(cwd string) map[string]any {
	return map[string]any{
		"rootDir":             cwd,
		"workspaceDir":        workspaceDirectory(cwd),
		"profile":             workassRuntimeProfile(),
		"version":             daemonVersion,
		"electron":            "",
		"node":                "",
		"name":                "workass",
		"daemon":              true,
		"sessionSaveMode":     "lean-payload-v2",
		"workspaceRebindMode": "transactional-v1",
	}
}

func workassRuntimeProfile() string {
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("WORKASS_PROFILE")))
	if profile != "dev" && profile != "test" {
		return "prod"
	}
	return profile
}

// workspaceDirectory preserves the path split exposed by the legacy Electron
// host: rootDir is the Workass repository/application directory, while
// workspaceDir is its parent (the directory that contains sibling projects).
func workspaceDirectory(rootDir string) string {
	clean := filepath.Clean(rootDir)
	return filepath.Dir(clean)
}

// readState is a frozen wire method retained for compatibility. The legacy
// work-queue integration it once read has been removed, so it reports an empty
// queue. The method name and reply shape are unchanged.
func readState(cwd string) map[string]any {
	return map[string]any{"runAt": nil, "items": []any{}}
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			if secretKeyRE.MatchString(k) {
				out[k] = "[redacted]"
				continue
			}
			out[k] = redactValue(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = redactValue(v)
		}
		return out
	default:
		return v
	}
}

func taskKinds() []string {
	return []string{"consulta", "review", "jira", "freeform", "chat"}
}

func defaultSettings() map[string]any {
	models := map[string]any{}
	permissionModes := map[string]any{}
	for _, kind := range taskKinds() {
		models[kind] = ""
		if kind != "chat" {
			permissionModes[kind] = ""
		}
	}
	return map[string]any{
		"version":            1,
		"models":             models,
		"permissionModes":    permissionModes,
		"chatMode":           "",
		"autoProcessEnabled": true,
		"notifications":      "none",
		"prApprover":         "",
		"modelScores":        acp.ModelScores{},
		"modelFavorites":     []any{},
	}
}

func defaultConfig() map[string]any {
	if redacted, ok := redactValue(defaultAppConfig()).(map[string]any); ok {
		return redacted
	}
	return defaultAppConfig()
}

func emptyPublicJob() any {
	return map[string]any{
		"id":               "",
		"kind":             "",
		"key":              nil,
		"title":            "",
		"status":           "",
		"startedAt":        "",
		"finishedAt":       nil,
		"code":             nil,
		"permissionMode":   "",
		"chatId":           nil,
		"tabId":            nil,
		"sessionId":        nil,
		"providerId":       nil,
		"result":           nil,
		"error":            nil,
		"stopReason":       nil,
		"crashInterrupted": false,
	}
}
