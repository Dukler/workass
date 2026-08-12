package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"workass/internal/chat"
	"workass/internal/fleet"
	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/machineid"
	providercontract "workass/internal/provider"
	"workass/internal/tlscert"
	"workass/internal/voice"
	"workass/internal/wire"
)

// daemonVersion is overridden by the release builders with -ldflags. Keep the
// development value for source builds and tests.
var daemonVersion = "0.0.1-dev"

var secretKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|bearer)`)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "fleet" {
		if err := runFleetCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "workass fleet: %v\n", err)
			os.Exit(1)
		}
		return
	}
	prod := flag.Bool("prod", prodModeDefault(), "use production daemon defaults where applicable; on Windows this defaults --port 80 and --bind lan")
	port := flag.Int("port", 8788, "HTTP/WebSocket port")
	bind := flag.String("bind", "localhost", "bind mode: localhost or lan")
	beacon := flag.Bool("beacon", beaconDefault(), "enable automatic LAN discovery; LAN binds announce and loopback binds listen without announcing")
	useTLS := flag.Bool("tls", true, "required: serve https/wss with this machine's own certificate, minted once into the state dir")
	rendererDir := flag.String("renderer-dir", "", "renderer directory override; empty serves embedded renderer2")
	mocksDirFlag := flag.String("mocks-dir", "", "design mocks directory override")
	acpCommand := flag.String("acp-command", "node", "ACP provider command")
	acpArgsJSON := flag.String("acp-args", `["desktop/acp/mock-server.mjs"]`, "ACP provider args as a JSON array")
	stateDirFlag := flag.String("state-dir", "state", "daemon state directory")
	headless := flag.Bool("headless", false, "run only the daemon; do not expect an Electron shell")
	installService := flag.Bool("install-service", false, "install and start the headless daemon as the current user's service")
	repairStartup := flag.Bool("repair-startup", false, "back up malformed startup state so the daemon can start again")
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
	if !*useTLS {
		fmt.Fprintln(os.Stderr, "plaintext Workass transport is no longer supported; remove --tls=false")
		os.Exit(2)
	}
	if *installService && !*headless {
		fmt.Fprintln(os.Stderr, "--install-service requires --headless")
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
	if *repairStartup {
		report, repairErr := repairStartupState(stateDir)
		if repairErr != nil {
			fmt.Fprintf(os.Stderr, "workass: repair startup state: %v\n", repairErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workass startup recovery complete (%d preserved files)\n", len(report.Moved))
		return
	}
	if *installService {
		if err := installHeadlessService(headlessServiceOptions{
			Executable: currentExecutablePath(),
			StateDir:   stateDir,
			Port:       *port,
			Bind:       *bind,
			Profile:    workassRuntimeProfile(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "workass: install headless service: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workass headless service installed and started (%s)\n", serviceName())
		return
	}
	_ = headless // The daemon is already UI-free; this flag documents the intended launch mode.
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
	identity, identityErr := machineid.Load(stateDir)
	if identityErr != nil {
		// A daemon with no provable identity still serves its own client; it
		// cannot own portable provider-lane or pairing identities.
		logger.Printf("[workass] machine identity unavailable: %v", identityErr)
	} else {
		logger.Printf("[workass] machine %s (%s)", identity.MachineID, identity.DisplayName)
	}
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
	certificate, certErr := tlscert.Ensure(stateDir)
	if certErr != nil {
		logger.Printf("[workass] tls: %v", certErr)
		os.Exit(1)
	}
	legacyAgentControlFile := filepath.Join(stateDir, "agent-control.json")
	if err := os.Remove(legacyAgentControlFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Printf("[workass] remove legacy agent control descriptor: %v", err)
	}
	mcpBaseURL := "https://mcp.localhost:" + strconv.Itoa(*port)
	acpManager := acp.NewManager(acp.Options{
		RootDir:                 cwd,
		StateDir:                stateDir,
		MachineID:               identity.MachineID,
		RuntimeProfile:          workassRuntimeProfile(),
		WorkassMCPBaseURL:       mcpBaseURL,
		WorkassMCPCACertFile:    filepath.Join(stateDir, tlscert.CertFileName),
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
		DeferProviderStartup:    true,
		SpareTTL:                *spareTTL,
		CompactionEnabled:       effectiveEngine.CompactionEnabled,
		CompactionThresholdPct:  effectiveEngine.CompactionThresholdPct,
		CompactionKeepLastTurns: effectiveEngine.CompactionKeepLastTurns,
		Logf: func(message string, fields map[string]any) {
			data, _ := json.Marshal(redactValue(fields))
			logger.Printf("[acp] %s %s", message, data)
		},
	})
	providerChats := newProviderChatRuntimeBeforeProviderStartup(acpManager, sessionState, stateDir, hub.Broadcast)
	if err := providerChats.StartupError(); err != nil {
		acpManager.Reset()
		logger.Printf("[workass] initialize authoritative chat runtime: %v", err)
		os.Exit(1)
	}
	chatControl := newChatControlCoordinator(acpManager, hub.Broadcast, providerChats)
	artifactHosting, err := artifacthost.New(stateDir, "https://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		acpManager.Reset()
		logger.Printf("[workass] initialize artifact hosting: %v", err)
		os.Exit(1)
	}
	artifactHosting.SetLogger(logger.Printf)
	channelCount := registerDaemonHandlers(hub, cwd, acpManager, daemonOptions{
		StateDir:            stateDir,
		ConfigPath:          appConfigPath,
		Engine:              effectiveEngine,
		EngineFlagOverrides: flagOverrides,
		ChatControl:         chatControl,
		ProviderChats:       providerChats,
		Artifacts:           artifactHosting,
	})
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
	logger.Printf("[workass] device state %s (trust-localhost=%v)", filepath.Join(stateDir, "devices.json"), *trustLocalhost)
	logger.Printf("[workass] provider registry %s", providersFile)

	handler := httpserve.New(staticDir, hub, nil)
	handler.MocksDir = mocksDir
	handler.ArtifactHosts = artifactHosting
	if staticDir == "" {
		handler.RendererFS = embeddedRendererFS()
	}
	handler.Version = daemonVersion
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
	handler.Metrics = func() map[string]any { return daemonMetrics(providerChats, stateDir, hub, acpManager) }
	agentControl, err := newAgentControlHandlerBeforeProviderStartup(acpManager, hub.Broadcast, chatControl)
	if err != nil {
		acpManager.Reset()
		logger.Printf("[workass] initialize agent control: %v", err)
		os.Exit(1)
	}
	agentControl.artifacts = artifactHosting
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			// Durable chat actors own the disposable provider-lane attachments.
			// Detach those actors before the manager tears down its bridge maps so
			// shutdown preserves the exact native-thread bindings for resume.
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := providerChats.Close(closeCtx); err != nil {
				logger.Printf("[workass] detach provider chat actors: %v", err)
			}
			cancel()
			acpManager.Reset()
		})
	}
	defer cleanup()
	shutdownRoot, requestShutdown := context.WithCancel(context.Background())
	defer requestShutdown()
	mux := http.NewServeMux()
	mux.HandleFunc("/workass/recovery/shutdown", localRecoveryShutdownHandler(requestShutdown))
	updateControl := newLocalUpdateControl(acpManager, requestShutdown)
	mux.HandleFunc("/workass/update/prepare", updateControl.prepare)
	mux.HandleFunc("/workass/update/commit", updateControl.commit)
	mux.HandleFunc("/workass/update/cancel", updateControl.cancel)
	mux.Handle(agentMCPPath, newAgentStatelessMCPHandler(acpManager, agentControl))
	mux.Handle(browserMCPPath, newBrowserStatelessMCPHandler(acpManager, defaultBrowserControlFile(stateDir), providerChats))
	mux.Handle(fleetQRPath, newFleetQRHandler(fleetKeys, *port, *bind, logger.Printf))
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
		mcpCertificates, err := tlscert.NewLoopbackServerCertificateRotator(certificate, "mcp.localhost")
		if err != nil {
			cleanup()
			logger.Printf("[workass] mint MCP loopback certificate: %v", err)
			os.Exit(1)
		}
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate.TLS},
			MinVersion:   tls.VersionTLS13,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if strings.EqualFold(strings.TrimSpace(hello.ServerName), "mcp.localhost") {
					return mcpCertificates.GetCertificate(hello)
				}
				return &certificate.TLS, nil
			},
		}
		listener = tls.NewListener(listener, server.TLSConfig)
		hub.SetTLSFingerprint(certificate.Fingerprint)
		tlsFingerprint = certificate.Fingerprint
		if certificate.Minted {
			logger.Printf("[workass] minted this machine's certificate in %s", stateDir)
		}
		logger.Printf("[workass] serving https/wss · certificate %s", certificate.Fingerprint[:16])
	}
	signalCtx, stopSignals := signal.NotifyContext(shutdownRoot, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErr := startDaemonHTTP(server, listener)
	readinessTLS, readinessErr := daemonReadinessTLSConfig(certificate, *useTLS)
	if readinessErr != nil {
		stopStartedDaemonHTTP(server, listener)
		cleanup()
		logger.Printf("[workass] prepare listener readiness probe: %v", readinessErr)
		os.Exit(1)
	}
	if err := releaseProviderStartupAfterHTTPReady(signalCtx, listener, readinessTLS, serveErr, func() error {
		acpManager.StartProviderStartup()
		acpManager.StartProviderDetection(signalCtx)
		return providerChats.ResumeActors()
	}); err != nil {
		stopStartedDaemonHTTP(server, listener)
		cleanup()
		logger.Printf("[workass] release provider startup after MCP readiness: %v", err)
		os.Exit(1)
	}
	startMachinePresence(signalCtx, machineBook, hub, identity, machinePresenceOptions{
		Bind:   *bind,
		Port:   *port,
		Beacon: *beacon,
	}, logger)
	if err := serveDaemonHTTPWithServeError(signalCtx, server, listener, cleanup, serveErr); err != nil {
		cleanup()
		logger.Printf("[workass] server stopped: %v", err)
		os.Exit(1)
	}
}

// localRecoveryShutdownHandler is deliberately outside the frozen WebSocket
// protocol.  It is a loopback-only shell recovery hook: after replying 202 it
// cancels the daemon's root context, allowing the ordinary graceful shutdown
// path to settle engines and release the listener.  A LAN peer can never use
// it to stop another machine.
func localRecoveryShutdownHandler(requestShutdown context.CancelFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"stopping":true}`))
		go func() {
			time.Sleep(25 * time.Millisecond)
			requestShutdown()
		}()
	}
}

type appUpdateDrainGate interface {
	BeginUpdateDrain() acp.AppUpdateReadiness
	CancelUpdateDrain()
}

type localUpdateControl struct {
	mu              sync.Mutex
	gate            appUpdateDrainGate
	requestShutdown context.CancelFunc
	preparedID      string
}

func newLocalUpdateControl(gate appUpdateDrainGate, requestShutdown context.CancelFunc) *localUpdateControl {
	return &localUpdateControl{gate: gate, requestShutdown: requestShutdown}
}

func updateRequestID(r *http.Request) string {
	var body struct {
		UpdateID string `json:"updateId"`
	}
	reader := io.LimitReader(r.Body, 4096)
	if err := json.NewDecoder(reader).Decode(&body); err != nil {
		return ""
	}
	id := strings.TrimSpace(body.UpdateID)
	if len(id) < 8 || len(id) > 96 {
		return ""
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return ""
		}
	}
	return id
}

func localUpdateRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return "", false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	id := updateRequestID(r)
	if id == "" {
		http.Error(w, "invalid update id", http.StatusBadRequest)
		return "", false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	return id, true
}

// prepare proves quiescence and fences all new work, but deliberately keeps
// the daemon alive. The shell first arms an independent worker; only then does
// commit stop the process. A worker launch failure can therefore cancel the
// fence without interrupting the running app.
func (control *localUpdateControl) prepare(w http.ResponseWriter, r *http.Request) {
	id, ok := localUpdateRequest(w, r)
	if !ok {
		return
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.preparedID != "" && control.preparedID != id {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ready":false,"reason":"another update is prepared"}`))
		return
	}
	if control.preparedID == id {
		_, _ = w.Write([]byte(`{"ready":true,"prepared":true}`))
		return
	}
	readiness := control.gate.BeginUpdateDrain()
	if !readiness.Ready {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(readiness)
		return
	}
	control.preparedID = id
	_ = json.NewEncoder(w).Encode(readiness)
}

func (control *localUpdateControl) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := localUpdateRequest(w, r)
	if !ok {
		return
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.preparedID != id {
		http.Error(w, "update is not prepared", http.StatusConflict)
		return
	}
	control.gate.CancelUpdateDrain()
	control.preparedID = ""
	_, _ = w.Write([]byte(`{"cancelled":true}`))
}

func (control *localUpdateControl) commit(w http.ResponseWriter, r *http.Request) {
	id, ok := localUpdateRequest(w, r)
	if !ok {
		return
	}
	control.mu.Lock()
	if control.preparedID != id {
		control.mu.Unlock()
		http.Error(w, "update is not prepared", http.StatusConflict)
		return
	}
	control.preparedID = ""
	control.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"stopping":true}`))
	go func() {
		time.Sleep(25 * time.Millisecond)
		control.requestShutdown()
	}()
}

func startDaemonHTTP(server *http.Server, listener net.Listener) chan error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	return serveErr
}

func stopStartedDaemonHTTP(server *http.Server, listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
	if server != nil {
		_ = server.Close()
	}
}

func daemonReadinessTLSConfig(certificate tlscert.Certificate, useTLS bool) (*tls.Config, error) {
	if !useTLS {
		return nil, nil
	}
	if len(certificate.TLS.Certificate) == 0 {
		return nil, errors.New("daemon certificate has no leaf")
	}
	root, err := x509.ParseCertificate(certificate.TLS.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse daemon certificate for readiness probe: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &tls.Config{
		RootCAs:    roots,
		ServerName: "mcp.localhost",
		MinVersion: tls.VersionTLS13,
	}, nil
}

func waitForDaemonHTTP(ctx context.Context, listener net.Listener, readinessTLS *tls.Config, serveErr <-chan error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if listener == nil {
		return errors.New("daemon listener is nil")
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || port == "" {
		return fmt.Errorf("daemon listener address is invalid: %v", listener.Addr())
	}
	scheme := "http"
	if readinessTLS != nil {
		scheme = "https"
	}
	probeURL := scheme + "://" + net.JoinHostPort("127.0.0.1", port) + agentMCPPath
	transport := &http.Transport{TLSClientConfig: readinessTLS}
	client := &http.Client{Transport: transport, Timeout: 250 * time.Millisecond}
	defer transport.CloseIdleConnections()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if requestErr != nil {
			return requestErr
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			return nil
		}
		select {
		case err := <-serveErr:
			if err == nil {
				return errors.New("daemon HTTP server stopped before readiness")
			}
			return fmt.Errorf("daemon HTTP server stopped before readiness: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("daemon listener readiness probe failed: %w", requestErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func releaseProviderStartupAfterHTTPReady(
	ctx context.Context,
	listener net.Listener,
	readinessTLS *tls.Config,
	serveErr <-chan error,
	release func() error,
) error {
	if err := waitForDaemonHTTP(ctx, listener, readinessTLS, serveErr); err != nil {
		return err
	}
	if release == nil {
		return errors.New("provider startup release is unavailable")
	}
	// Provider sessions receive the stateless MCP descriptors during
	// session/new/session/resume. Nothing provider-owned may be released until
	// the exact listener those descriptors name has answered this probe.
	return release()
}

func serveDaemonHTTP(ctx context.Context, server *http.Server, listener net.Listener, cleanup func()) error {
	return serveDaemonHTTPWithServeError(ctx, server, listener, cleanup, startDaemonHTTP(server, listener))
}

func serveDaemonHTTPWithServeError(ctx context.Context, server *http.Server, listener net.Listener, cleanup func(), serveErr <-chan error) error {
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

// daemonEventBroadcaster preserves manager emission order at the frozen wire
// boundary. Chat semantics have already crossed the durable actor ingress in
// Manager.emit; this function must never recover an unowned event by writing it
// into the retired renderer/session mirror. Non-chat executor events remain
// transient publications and acquire no chat persistence here.
func daemonEventBroadcaster(_ *sessionStore, broadcast func(string, any)) func(string, any) {
	var dispatchMu sync.Mutex
	return func(channel string, payload any) {
		// Manager timers/provider callbacks may emit concurrently. Keep visible
		// delivery ordered without manufacturing another semantic owner.
		dispatchMu.Lock()
		if broadcast != nil {
			broadcast(channel, payload)
		}
		dispatchMu.Unlock()
	}
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
		return listServerDirectories(requested), nil
	})
	count++
	hub.Register("fs:create-dir", func(args []any) (any, error) {
		arg := firstMapArg(args)
		return createServerDirectory(fieldString(arg, "parent"), fieldString(arg, "name")), nil
	})
	count++

	for _, def := range daemonStubs() {
		registerStub(hub, def.channel, def.result)
		count++
	}
	sessionState := sharedSessionStore(state.stateDir)
	providerChats := opts.ProviderChats
	registerSessionHandlersWithActor(hub, sessionState, acpManager, providerChats)
	count += 2
	registerStateDigestHandler(hub, providerChats, acpManager, state.setting)
	count++
	registerArchiveHandlers(hub, state, providerChats)
	count += 2
	registerVisualizeHandler(hub, opts.Artifacts, providerChats, state.stateDir)
	count++
	registerNotifyHandlers(hub)
	count++
	registerConfigSettingsHandlers(hub, state, acpManager)
	count += 4
	registerVoiceHandlers(hub)
	count += 2
	if acpManager != nil {
		chatControl := opts.ChatControl
		if chatControl == nil {
			chatControl = newChatControlCoordinator(acpManager, hub.Broadcast, providerChats)
		}
		chatControl.providerChats = providerChats
		registerAcpHandlers(hub, acpManager, state.stateDir, sessionState, chatControl, providerChats)
		count += 23
	}
	return count
}

func registerStateDigestHandler(hub *wire.Hub, providerChats *providerChatRuntime, manager *acp.Manager, settings *appSettingsStore) {
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
		if providerChats == nil {
			return nil, errors.New("state digest requires the authoritative chat actor runtime")
		}
		return providerChats.StateDigest(catalogHashes, stateDigestHash(settingsValue), stateDigestHash(processes))
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

func registerSessionHandlersWithActor(hub *wire.Hub, store *sessionStore, manager *acp.Manager, providerChats *providerChatRuntime) {
	hub.Register("session:get", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("session:get requires the authoritative chat actor runtime")
		}
		raw, err := providerChats.ProjectSessionRaw()
		if err != nil {
			return nil, err
		}
		return wire.RawResult(raw), nil
	})
	hub.Register("session:save", func(args []any) (any, error) {
		var snapshot any = map[string]any{}
		if len(args) > 0 && args[0] != nil {
			snapshot = args[0]
		}
		saved := false
		var err error
		if providerChats == nil {
			return false, errors.New("session:save requires the authoritative chat actor runtime")
		}
		saved, err = providerChats.ApplyRendererSnapshot(snapshot)
		if err != nil {
			return false, err
		}
		if saved {
			hub.Broadcast("agent:apply", map[string]any{"action": "session-refresh"})
		}
		if !saved {
			return map[string]any{"ok": false}, nil
		}
		global := store.GlobalSnapshot()
		return map[string]any{"ok": true, globalPresentationRevisionField: intValue(global[globalPresentationRevisionField])}, nil
	})
	hub.Register("chat:queue-replace", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("chat queue requires the authoritative actor runtime")
		}
		arg := firstMapArg(args)
		return providerChats.ReplaceStagedQueue(
			fieldString(arg, "tabId"), fieldString(arg, "chatId"),
			providercontract.NormalizeOperationID(fieldString(arg, "operationId")), uint64(max(0, intValue(arg["expectedRevision"]))),
			anySlice(arg["queue"]),
		)
	})
	hub.Register("chat:create", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("chat creation requires the authoritative actor runtime")
		}
		return providerChats.CreateRendererChat(firstMapArg(args))
	})
	hub.Register("chat:presentation-save", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("chat presentation requires the authoritative actor runtime")
		}
		arg := firstMapArg(args)
		return providerChats.SavePresentation(
			fieldString(arg, "tabId"), fieldString(arg, "chatId"),
			providercontract.NormalizeOperationID(fieldString(arg, "operationId")), uint64(max(0, intValue(arg["expectedRevision"]))), arg,
		)
	})
	hub.Register("chat:runtime-controls-save", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("chat runtime controls require the authoritative actor runtime")
		}
		arg := firstMapArg(args)
		return providerChats.SaveRuntimeControls(
			fieldString(arg, "tabId"), fieldString(arg, "chatId"),
			providercontract.NormalizeOperationID(fieldString(arg, "operationId")), uint64(max(0, intValue(arg["expectedRevision"]))), arg,
		)
	})
	hub.Register("chat:delete", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("chat deletion requires the authoritative actor runtime")
		}
		arg := firstMapArg(args)
		operationID := providercontract.NormalizeOperationID(fieldString(arg, "operationId"))
		force, _ := boolField(arg, "force")
		if err := providerChats.DeleteChat(context.Background(), fieldString(arg, "tabId"), fieldString(arg, "chatId"), operationID, force); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "operationId": string(operationID)}, nil
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

func registerAcpHandlers(hub *wire.Hub, manager *acp.Manager, stateDir string, sessionState *sessionStore, chatControl *chatControlCoordinator, providerChats *providerChatRuntime) {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = manager.StateDir()
	}
	hub.SetOnClientReady(func(send func(channel string, payload any) error) {
		manager.ReplayProviderEvents(send)
		_ = send("agent:apply", map[string]any{"action": "session-refresh"})
	})
	hub.SetOnControllerReady(func(send func(channel string, payload any) error) {
		if providerChats == nil {
			return
		}
		_ = providerChats.ReplayPendingPermissions(send)
	})
	hub.Register("app-chat:new-session", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if refresh, _ := boolField(arg, "refreshPlanUsage"); refresh {
			return nil, errors.New("chat-scoped plan refresh is unavailable after actor cutover; use app-chat:refresh-plan-usage")
		}
		oldSessionID := fieldString(arg, "replaceSessionId")
		workspaceRebind, _ := boolField(arg, "workspaceRebind")
		if providerChats == nil {
			return map[string]any{"error": "app-chat:new-session requires the durable chat actor", "models": []any{}, "modes": []any{}}, nil
		}
		if oldSessionID != "" || workspaceRebind {
			result, err := providerChats.MoveWorkspace(context.Background(), arg)
			if err != nil {
				return map[string]any{"error": err.Error(), "models": []any{}, "modes": []any{}, "workspaceCommitted": false, "workspaceRebound": false}, nil
			}
			return result, nil
		}
		if fieldString(arg, "tabId") == "" || fieldString(arg, "chatId") == "" {
			return map[string]any{"error": "app-chat:new-session requires exact tabId and chatId", "models": []any{}, "modes": []any{}}, nil
		}
		// Once a workspace move has committed, the daemon snapshot owns cwd. A
		// stale/reconnected controller may still ask to create the replacement
		// session using its pre-move cwd; never let that recreate the provider
		// thread in the old directory.
		if cwd, _, ok, workspaceErr := providerChats.ChatWorkspaceForExactPair(fieldString(arg, "tabId"), fieldString(arg, "chatId")); workspaceErr != nil {
			return map[string]any{"error": workspaceErr.Error(), "models": []any{}, "modes": []any{}}, nil
		} else if ok {
			arg["cwd"] = cwd
		}
		info, err := providerChats.SelectNewChat(context.Background(), arg)
		if err != nil {
			return map[string]any{"error": err.Error(), "models": []any{}, "modes": []any{}}, nil
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
		if providerChats == nil {
			return nil, errors.New("app-chat:fork requires the durable chat actor")
		}
		return providerChats.Fork(context.Background(), arg)
	})
	hub.Register("app-chat:close-session", func(args []any) (any, error) {
		// Closing the disposable host attachment never deletes the immutable
		// provider lane. Exact resume remains the only later reattachment path.
		if providerChats == nil {
			return nil, errors.New("app-chat:close-session requires the durable chat actor")
		}
		return providerChats.CloseSession(context.Background(), stringArg(args, 0)), nil
	})
	hub.Register("app-chat:reset", func(args []any) (any, error) {
		return nil, errors.New("global session reset is unavailable after durable chat cutover")
	})
	hub.Register("app-chat:set-model", func(args []any) (any, error) {
		return nil, errors.New("session-addressed model changes are retired; use chat:runtime-controls-save with exact tabId, chatId, revision, and operationId")
	})
	hub.Register("app-chat:set-mode", func(args []any) (any, error) {
		return nil, errors.New("session-addressed mode changes are retired; use chat:runtime-controls-save with exact tabId, chatId, revision, and operationId")
	})
	hub.Register("app-chat:steer", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if providerChats == nil {
			return nil, errors.New("steering requires the durable chat actor")
		}
		if result, handled, err := providerChats.Steer(context.Background(), arg); handled {
			if err == nil && boolFieldValue(result, "daemonQueued") && chatControl != nil {
				if live, ok := manager.LiveSession(fieldString(arg, "sessionId")); ok {
					chatControl.refresh(live.TabID, live.ChatID, false)
				}
			}
			return result, err
		}
		return nil, errors.New("steer does not belong to an actor-owned session")
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
		return providerChats.ChatCommands(fieldString(arg, "tabId"), fieldString(arg, "chatId"))
	})
	hub.Register("spawned-work:list", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
		if tabID == "" || chatID == "" {
			return nil, errors.New("spawned-work:list requires {tabId, chatId}")
		}
		if providerChats == nil {
			return nil, errors.New("spawned-work:list requires the durable chat actor")
		}
		items, err := providerChats.ListBackground(tabID, chatID)
		if err != nil {
			return nil, err
		}
		reply := map[string]any{"items": items}
		obligation, err := providerChats.Obligation(tabID, chatID)
		if err != nil {
			return nil, err
		}
		if obligation != nil {
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
		if providerChats == nil {
			return nil, errors.New("spawned-work:read requires the durable chat actor")
		}
		return providerChats.ReadBackground(tabID, chatID, id, intField(arg, "tailBytes"))
	})
	// The stop square. Mutating, so the wire already requires the controller
	// lease before this handler is reached — the same bar proc:kill sits behind.
	hub.Register("spawned-work:stop", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID, chatID, id := fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "id")
		if tabID == "" || chatID == "" || id == "" {
			return nil, errors.New("spawned-work:stop requires {tabId, chatId, id}")
		}
		if providerChats == nil {
			return nil, errors.New("spawned-work:stop requires the durable chat actor")
		}
		return providerChats.RunBackgroundAction(context.Background(), tabID, chatID, chat.BackgroundAction{
			Kind: chat.BackgroundStopWork, OperationID: stableBackgroundOperationID(chat.BackgroundStopWork, tabID, chatID, id),
			Stop: &chat.StopWorkAction{WorkID: id},
		})
	})
	hub.Register("job:start", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if providerChats == nil || fieldString(arg, "tabId") == "" || fieldString(arg, "chatId") == "" {
			return nil, errors.New("job:start requires an exact durable chat actor")
		}
		// Workspace cwd is daemon-authoritative after a transactional sidebar
		// move. A stale controller may still submit the previous value, but it
		// can never make the next job execute there.
		if cwd, _, ok, err := providerChats.ChatWorkspaceForExactPair(fieldString(arg, "tabId"), fieldString(arg, "chatId")); err != nil {
			return nil, err
		} else if ok {
			arg["cwd"] = cwd
		}
		job, err := providerChats.Start(context.Background(), arg, "human")
		if err == nil && boolFieldValue(job, "queued") && chatControl != nil {
			tabID, chatID := fieldString(arg, "tabId"), fieldString(arg, "chatId")
			chatControl.refresh(tabID, chatID, false)
		}
		return job, err
	})
	hub.Register("job:cancel", func(args []any) (any, error) {
		jobID := stringArg(args, 0)
		if providerChats == nil {
			return nil, errors.New("job:cancel requires the durable chat actor")
		}
		if result, handled, err := providerChats.Cancel(context.Background(), jobID); handled {
			return result, err
		}
		return nil, errors.New("job cancellation does not belong to an actor-owned turn")
	})
	hub.Register("chat:permission-decide", func(args []any) (any, error) {
		arg := firstMapArg(args)
		id := fieldString(arg, "id")
		if id == "" {
			return map[string]any{"ok": false}, nil
		}
		if providerChats == nil {
			return nil, errors.New("permission decisions require the durable chat actor")
		}
		if accepted, handled, err := providerChats.ResolvePermission(context.Background(), id, fieldString(arg, "optionId")); handled {
			return map[string]any{"ok": accepted}, err
		}
		return nil, errors.New("permission does not belong to an actor-owned request")
	})
	hub.Register("chat:permissions-pending", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, errors.New("permission replay requires the durable chat actor")
		}
		permissions, err := providerChats.PendingPermissions()
		return map[string]any{"permissions": permissions}, err
	})
	hub.Register("chat:env-get", func(args []any) (any, error) {
		arg := firstMapArg(args)
		chatID := fieldString(arg, "chatId")
		tabID := fieldString(arg, "tabId")
		if chatID == "" && tabID == "" {
			chatID = stringArg(args, 0)
		}
		if providerChats == nil {
			return nil, errors.New("chat:env-get requires the durable chat actor")
		}
		return providerChats.ChatEnvGet(tabID, chatID)
	})
	hub.Register("chat:checkpoints", func(args []any) (any, error) {
		arg := firstMapArg(args)
		chatID := fieldString(arg, "chatId")
		tabID := fieldString(arg, "tabId")
		if chatID == "" && tabID == "" {
			chatID = stringArg(args, 0)
		}
		if providerChats == nil {
			return nil, errors.New("chat:checkpoints requires the durable chat actor")
		}
		return providerChats.ChatCheckpoints(tabID, chatID)
	})
	hub.Register("chat:rewind", func(args []any) (any, error) {
		arg := firstMapArg(args)
		result, err := providerChats.Rewind(
			context.Background(), fieldString(arg, "tabId"), fieldString(arg, "chatId"), intField(arg, "turnSeq"),
			providercontract.OperationID(fieldString(arg, "operationId")),
		)
		if err == nil && chatControl != nil {
			chatControl.refresh(fieldString(arg, "tabId"), fieldString(arg, "chatId"), false)
		}
		return result, err
	})
	hub.Register("chat:diff", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if providerChats == nil {
			return nil, errors.New("chat:diff requires the durable chat actor")
		}
		return providerChats.ChatDiff(context.Background(), fieldString(arg, "tabId"), fieldString(arg, "chatId"), fieldString(arg, "repo"), fieldString(arg, "path"))
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
		CWD:         fieldString(m, "cwd"),
		BridgeKey:   fieldString(m, "bridgeKey"),
		TabID:       firstNonEmptyString(fieldString(m, "tabId"), fieldString(m, "chatTabId")),
		ChatID:      fieldString(m, "chatId"),
		SessionID:   fieldString(m, "sessionId"),
		OperationID: providercontract.NormalizeOperationID(fieldString(m, "operationId")),
		ProviderID:  fieldString(m, "providerId"),
		ModelID:     firstNonEmptyString(fieldString(m, "modelId"), fieldString(m, "currentModelId"), fieldString(m, "model")),
		ModeID:      firstNonEmptyString(fieldString(m, "modeId"), fieldString(m, "currentModeId"), fieldString(m, "mode")),
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
func listServerDirectories(requested string) map[string]any {
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
	target := requested
	if strings.TrimSpace(target) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return empty(nil, nil, []directoryEntry{}, err)
		}
		target = home
	}

	abs, err := filepath.Abs(target)
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

// createServerDirectory creates one direct child of the exact server directory
// currently shown by the picker. It deliberately accepts a name rather than a
// client-composed path so a LAN/browser client cannot smuggle traversal through
// a path separator that means something different on the daemon's OS.
func createServerDirectory(parentDir, requestedName string) map[string]any {
	name := strings.TrimSpace(requestedName)
	result := func(parent, path any, err error) map[string]any {
		out := map[string]any{"name": name, "path": path, "parent": parent}
		if err != nil {
			out["error"] = acp.RedactSensitiveText(err.Error())
		}
		return out
	}
	if name == "" {
		return result(parentDir, nil, errors.New("Escribí un nombre para la carpeta."))
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return result(parentDir, nil, errors.New("Usá un solo nombre, sin separadores de ruta."))
	}
	parentDir = strings.TrimSpace(parentDir)
	if parentDir == "" {
		return result(nil, nil, errors.New("Elegí primero la carpeta contenedora."))
	}
	parent, err := filepath.Abs(parentDir)
	if err != nil {
		return result(parentDir, nil, err)
	}
	parent = filepath.Clean(parent)
	info, err := os.Stat(parent)
	if err != nil {
		return result(parent, nil, err)
	}
	if !info.IsDir() {
		return result(parent, nil, fmt.Errorf("%s no es una carpeta", parent))
	}
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		return result(parent, nil, err)
	}
	return result(parent, target, nil)
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
