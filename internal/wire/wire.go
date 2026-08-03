package wire

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workass/internal/fleet"
	"workass/internal/lease"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var mutatingChannels = map[string]struct{}{
	"lan:access-decide":             {},
	"lan:revoke":                    {},
	"settings:set":                  {},
	"config:set":                    {},
	"session:save":                  {},
	"chat:archive-append":           {},
	"chat:rewind":                   {},
	"activity:clear":                {},
	"teams:refresh":                 {},
	"jira:sync":                     {},
	"deploy:auth":                   {},
	"job:start":                     {},
	"job:cancel":                    {},
	"chat:kill-terminal":            {},
	"chat:kill-command":             {},
	"app-chat:steer":                {},
	"app-chat:use-rate-limit-reset": {},
	"app-chat:reset":                {},
	"app-chat:detect-acp":           {},
	"app-chat:new-session":          {},
	"app-chat:refresh-plan-usage":   {},
	"app-chat:fork":                 {},
	"app-chat:close-session":        {},
	"app-chat:set-model":            {},
	"app-chat:set-mode":             {},
	"providers:detect":              {},
	"providers:update":              {},
	"providers:toggle":              {},
	"proc:kill":                     {},
	"proc:kill-all":                 {},
	// Stopping background work kills processes on this machine, so it belongs
	// with proc:kill rather than with the read-only spawned-work channels an
	// approved device may call while another device holds the lease.
	"spawned-work:stop":             {},
	"agent-proc:kill":               {},
	"code:unlock":                   {},
	"code:lock":                     {},
	"chat:permission-decide":        {},
	// `chat:permissions-pending` used to sit here, on the reasoning that
	// permission titles belong to the surface that can answer them. That
	// conflated seeing with deciding. An approved device already reads every
	// message of every chat through session:get, so withholding the question an
	// agent is asking protects nothing — it only blinds a watching phone, whose
	// whole reason to exist is showing you a card raised at 3am. The lease is
	// there so two devices cannot answer the same card; `chat:permission-decide`
	// above still enforces exactly that.
	"draft:save":         {},
	"status:set":         {},
	"clipboard:write":    {},
	"notify":             {},
	"teams:share-link":   {},
	"external:open":      {},
	"review:open":        {},
	"browser:set-active": {},
}

// Events only the controller may see. The test is not "is this sensitive" —
// an approved device reads the whole session anyway — but "does this address
// the screen a human is sitting at". `notify` and `show` put something on that
// screen, and `lan:access-request` asks its owner to admit a new device.
//
// `chat:permission-request` and `chat:permission-resolved` deliberately left:
// they describe what an agent is doing, which every approved device may watch.
// Deciding stays exclusive through `chat:permission-decide` in mutatingChannels.
var controllerOnlyEventChannels = map[string]struct{}{
	"lan:access-request": {},
	"notify":             {},
	"show":               {},
}

const defaultAccessRequestTimeout = 120 * time.Second
const defaultDeviceRefreshInterval = 30 * time.Second
const notifyBacklogLimit = 20
const outboundQueueFrameLimit = 128
const outboundQueueByteLimit = 16 << 20

// session:get carries the full canonical snapshot in one reply frame, and real
// profiles have exceeded 64 MiB (69 MiB observed 2026-07-24). A limit below the
// snapshot size strands every controller in a hydrate→drop→reconnect loop, so
// the ceiling must stay far above any plausible snapshot until hydration is
// chunked or leaned.
const outboundFrameByteLimit = 256 << 20

// A frame larger than the whole regular byte budget (a snapshot reply) is
// "bulk": one may be queued at a time and it does not consume the regular
// budget, so a draining snapshot and live events stop evicting each other
// (iPhone hydration drops, 2026-07-27 21:52 and 22:56).
func outboundFrameIsBulk(n int) bool { return n > outboundQueueByteLimit }

// The write deadline scales with payload: a phone on wifi needs tens of
// seconds to drink a 19 MiB snapshot, and the old flat 5s deadline executed
// it mid-hydration. A stalled peer still dies at the floor rate.
const writeDeadlineBase = 5 * time.Second
const writeDeadlineFloorBytesPerSec = 512 << 10

func writeTimeoutForPayload(n int) time.Duration {
	return writeDeadlineBase + time.Duration(n)*time.Second/writeDeadlineFloorBytesPerSec
}

var errOutboundQueueFull = errors.New("outbound websocket queue full")
var errOutboundFrameTooLarge = errors.New("outbound websocket frame too large")
var errClientClosed = errors.New("websocket client closed")
var errControllerChanged = errors.New("websocket client is no longer controller")

// Handler is the positional-args form used by the frozen LAN invoke protocol.
type Handler func(args []any) (any, error)

// RawResult is a trusted pre-serialized JSON result. The WebSocket reply path
// can wrap it in the frozen reply envelope and frame in the same backing
// buffer, avoiding a second snapshot-sized allocation.
type RawResult []byte

func (r RawResult) MarshalJSON() ([]byte, error) {
	if !json.Valid(r) {
		return nil, errors.New("wire raw result is not valid JSON")
	}
	return r, nil
}

// Options configures optional P3 pairing/controller behavior for a hub.
type Options struct {
	Lease                 *lease.Manager
	TrustLocalhost        bool
	AccessRequestTimeout  time.Duration
	DeviceRefreshInterval time.Duration
	OnClientReady         func(send func(channel string, payload any) error)
	OnControllerReady     func(send func(channel string, payload any) error)
	Logf                  func(format string, args ...any)
}

const (
	// A challenge is good for one attempt inside this window. Long enough for a
	// human-speed paste on a phone, short enough that a captured nonce is stale
	// before anyone could use it.
	fleetChallengeTTL = 2 * time.Minute
	// Guessing a 128-bit key is not a threat model, but a socket that keeps
	// failing is not enrolling either; make it reconnect rather than grind.
	fleetMaxFailures = 5
)

// Hub owns the invoke handler registry and connected WebSocket clients.
type Hub struct {
	mu                sync.RWMutex
	handlers          map[string]Handler
	clients           map[*client]struct{}
	pending           map[string]*pendingAccess
	requestSeq        uint64
	lease             *lease.Manager
	fleetKeys         *fleet.Store
	machineID         string
	instanceID        string
	tlsFingerprint    string
	trustLocalhost    bool
	accessTimeout     time.Duration
	deviceRefresh     time.Duration
	onClientReady     func(send func(channel string, payload any) error)
	onControllerReady func(send func(channel string, payload any) error)
	logf              func(format string, args ...any)
	notifyBacklog     []any
	stats             hubStats
	channelMu         sync.Mutex
	invokeStats       map[string]*channelStat
	eventStats        map[string]*channelStat
}

type client struct {
	conn             net.Conn
	outMu            sync.Mutex
	outbound         chan outboundFrame
	outboundBytes    int
	// Bytes of the queued bulk frame, if any; see outboundFrameIsBulk.
	outboundBulkBytes int
	done             chan struct{}
	closeOnce        sync.Once
	stateMu          sync.RWMutex
	ip               string
	deviceName       string
	userAgent        string
	device           *lease.Device
	issuedToken      string
	pendingRequestID string
	access           accessState
	fleetNonce       string
	fleetNonceAt     time.Time
	fleetFailures    int
	// Set when acting on this socket moved the controller lease, so the reply
	// path announces the handover the same way an explicit lan:take-control does.
	tookControl atomic.Bool
}

type outboundFrame struct {
	payload        []byte
	written        chan error
	controllerOnly bool
}

type pendingAccess struct {
	requestID   string
	ip          string
	deviceName  string
	userAgent   string
	requestedAt time.Time
	client      *client
	timer       *time.Timer
}

type accessState struct {
	State       string
	RequestID   string
	Reason      string
	RequestedAt string
}

// NewHub creates an empty wire hub.
func NewHub(options ...Options) *Hub {
	opts := Options{TrustLocalhost: true}
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.AccessRequestTimeout <= 0 {
		opts.AccessRequestTimeout = defaultAccessRequestTimeout
	}
	if opts.DeviceRefreshInterval <= 0 {
		opts.DeviceRefreshInterval = defaultDeviceRefreshInterval
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	hub := &Hub{
		instanceID:        newInstanceID(),
		handlers:          make(map[string]Handler),
		clients:           make(map[*client]struct{}),
		pending:           make(map[string]*pendingAccess),
		lease:             opts.Lease,
		trustLocalhost:    opts.TrustLocalhost,
		accessTimeout:     opts.AccessRequestTimeout,
		deviceRefresh:     opts.DeviceRefreshInterval,
		onClientReady:     opts.OnClientReady,
		onControllerReady: opts.OnControllerReady,
		logf:              opts.Logf,
	}
	if hub.lease != nil && hub.deviceRefresh > 0 {
		go hub.deviceRefreshLoop()
	}
	return hub
}

// newInstanceID names one run of one daemon. It is not persisted on purpose:
// its whole meaning is "this process", and a value that survived a restart would
// say the opposite of what a client needs to know.
func newInstanceID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// A clock is a poor unique id but a fine change detector, which is all
		// this is: two runs must not look like one.
		return "i-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "i-" + hex.EncodeToString(raw)
}

// InstanceID is this daemon run's id, stamped onto every access-state event.
func (h *Hub) InstanceID() string { return h.instanceID }

// SetTLSFingerprint tells the hub which certificate it is being served behind,
// so an enrolling client can verify it under the fleet key (E5).
func (h *Hub) SetTLSFingerprint(fingerprint string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tlsFingerprint = strings.TrimSpace(fingerprint)
}

// SetMachineID names the machine on the socket. SetFleet also carries the id
// because a proof binds to it; this exists for a daemon whose fleet file could
// not be read, so a client still learns which machine answered.
func (h *Hub) SetMachineID(machineID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.machineID = strings.TrimSpace(machineID)
}

// Register installs or replaces a channel handler.
func (h *Hub) Register(channel string, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[channel] = fn
}

// SetOnClientReady installs a hook called once a WebSocket client is approved
// and can receive normal daemon events.
func (h *Hub) SetOnClientReady(fn func(send func(channel string, payload any) error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onClientReady = fn
}

// SetOnControllerReady installs a hook called whenever an approved connection
// is the active controller, including after an explicit takeover. It is the
// reconnect seam for controller-only state such as pending permissions.
func (h *Hub) SetOnControllerReady(fn func(send func(channel string, payload any) error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onControllerReady = fn
}

// Invoke calls a registered handler with already-spread positional args.
//
// A handler panic is contained here and returned as an error to the one caller
// that triggered it. The daemon is the process every chat's engine hangs off:
// letting a panic escape kills all of them mid-turn and, under launchd
// KeepAlive, reads to the user as an unexplained restart. Failing one invoke is
// strictly cheaper, and the stack still reaches the log so the bug stays
// findable rather than silently swallowed.
func (h *Hub) Invoke(channel string, args []any) (result any, err error) {
	h.mu.RLock()
	fn := h.handlers[channel]
	h.mu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("unknown channel: %s", channel)
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			h.logf("[wire] handler panic channel=%s err=%v\n%s", channel, recovered, debug.Stack())
			result, err = nil, fmt.Errorf("wire: handler for %s panicked: %v", channel, recovered)
		}
		h.recordChannel(&h.invokeStats, channel, time.Since(started), 0)
	}()
	result, err = fn(args)
	return result, err
}

// Broadcast sends an event frame to every approved client. Ordinary delivery
// is queue-only, so one stalled socket cannot hold the daemon's visible stream;
// controller-only delivery waits for the actual write to preserve authorization
// and notification durability semantics.
func (h *Hub) Broadcast(channel string, payload any) {
	if _, ok := controllerOnlyEventChannels[channel]; ok && h.lease != nil {
		delivered, enqueueElapsed := h.broadcastToController(channel, payload)
		h.recordEventEnqueue(enqueueElapsed)
		if channel == "notify" && delivered == 0 {
			h.QueueNotify(payload)
		}
		return
	}
	started := time.Now()
	h.broadcastWhere(channel, payload, nil)
	h.recordEventEnqueue(time.Since(started))
}

// recordEventEnqueue measures JSON/frame preparation plus outbound queue
// admission. It deliberately excludes socket-writer completion; ordinary event
// delivery is asynchronous, so claiming this duration as wire delivery would
// overstate what the metric proves.
func (h *Hub) recordEventEnqueue(elapsed time.Duration) {
	atomic.AddUint64(&h.stats.broadcasts, 1)
	atomic.AddUint64(&h.stats.enqueueNanos, uint64(elapsed))
	for {
		previous := atomic.LoadUint64(&h.stats.enqueueMaxNanos)
		if uint64(elapsed) <= previous || atomic.CompareAndSwapUint64(&h.stats.enqueueMaxNanos, previous, uint64(elapsed)) {
			break
		}
	}
	if elapsed >= slowBroadcastThreshold {
		atomic.AddUint64(&h.stats.slowEnqueues, 1)
	}
}

const slowBroadcastThreshold = 50 * time.Millisecond

// channelStat accumulates per-channel cost. Invoke timings matter because the
// renderer posts a full session snapshot every 600ms: if that handler holds a
// lock for hundreds of milliseconds, streamed text queues behind it, and the
// only visible symptom is "the app is slow".
type channelStat struct {
	count      uint64
	totalNanos uint64
	maxNanos   uint64
	over50ms   uint64
	bytes      uint64
}

func (c *channelStat) observe(elapsed time.Duration, bytes int) {
	c.count++
	c.totalNanos += uint64(elapsed)
	if uint64(elapsed) > c.maxNanos {
		c.maxNanos = uint64(elapsed)
	}
	if elapsed >= slowBroadcastThreshold {
		c.over50ms++
	}
	c.bytes += uint64(bytes)
}

func topChannels(stats map[string]*channelStat, limit int) []map[string]any {
	type row struct {
		channel string
		stat    channelStat
	}
	rows := make([]row, 0, len(stats))
	for channel, stat := range stats {
		rows = append(rows, row{channel: channel, stat: *stat})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].stat.totalNanos > rows[b].stat.totalNanos })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, item := range rows {
		average := 0.0
		if item.stat.count > 0 {
			average = float64(item.stat.totalNanos) / float64(item.stat.count) / 1e6
		}
		entry := map[string]any{
			"channel":  item.channel,
			"count":    item.stat.count,
			"avgMs":    average,
			"maxMs":    float64(item.stat.maxNanos) / 1e6,
			"over50ms": item.stat.over50ms,
		}
		if item.stat.bytes > 0 {
			entry["bytes"] = item.stat.bytes
		}
		out = append(out, entry)
	}
	return out
}

// hubStats counters are atomic and unsynchronized with the hub lock: a
// diagnostics read must never be able to stall event delivery.
type hubStats struct {
	broadcasts      uint64
	enqueueNanos    uint64
	enqueueMaxNanos uint64
	slowEnqueues    uint64
	drops           uint64
}

func (h *Hub) recordChannel(target *map[string]*channelStat, channel string, elapsed time.Duration, bytes int) {
	h.channelMu.Lock()
	defer h.channelMu.Unlock()
	if *target == nil {
		*target = make(map[string]*channelStat)
	}
	stat := (*target)[channel]
	if stat == nil {
		stat = &channelStat{}
		(*target)[channel] = stat
	}
	stat.observe(elapsed, bytes)
}

// Stats reports event enqueue, channel, and connection counters for
// GET /workass/metrics.
func (h *Hub) Stats() map[string]any {
	broadcasts := atomic.LoadUint64(&h.stats.broadcasts)
	totalNanos := atomic.LoadUint64(&h.stats.enqueueNanos)
	averageMs := 0.0
	if broadcasts > 0 {
		averageMs = float64(totalNanos) / float64(broadcasts) / 1e6
	}
	h.mu.RLock()
	clients := len(h.clients)
	h.mu.RUnlock()
	h.channelMu.Lock()
	invokes := topChannels(h.invokeStats, 8)
	events := topChannels(h.eventStats, 8)
	h.channelMu.Unlock()
	maxMs := float64(atomic.LoadUint64(&h.stats.enqueueMaxNanos)) / 1e6
	over50 := atomic.LoadUint64(&h.stats.slowEnqueues)
	return map[string]any{
		"clients":          clients,
		"broadcasts":       broadcasts,
		"eventTimingScope": "enqueue",
		"enqueueAvgMs":     averageMs,
		"enqueueMaxMs":     maxMs,
		"enqueuesOver50ms": over50,
		// Compatibility aliases: these old identifiers also mean enqueue cost;
		// they do not claim controller-socket delivery.
		"broadcastAvgMs":     averageMs,
		"broadcastMaxMs":     maxMs,
		"broadcastsOver50ms": over50,
		"clientDrops":        atomic.LoadUint64(&h.stats.drops),
		"slowestInvokes":     invokes,
		"eventChannels":      events,
	}
}

func (h *Hub) QueueNotify(payloads ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifyBacklog = append(h.notifyBacklog, payloads...)
	if len(h.notifyBacklog) > notifyBacklogLimit {
		h.notifyBacklog = append([]any(nil), h.notifyBacklog[len(h.notifyBacklog)-notifyBacklogLimit:]...)
	}
}

func (h *Hub) HasControllerConnections() bool {
	return h.controllerConnectionCount() > 0
}

func (h *Hub) controllerConnectionCount() int {
	h.mu.RLock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	count := 0
	for _, c := range clients {
		if h.isControllerClient(c) {
			count++
		}
	}
	return count
}

func (h *Hub) broadcastToController(channel string, payload any) (int, time.Duration) {
	encodeStarted := time.Now()
	frame, err := json.Marshal(eventFrame{T: "event", Channel: channel, Payload: payload})
	if err != nil {
		return 0, time.Since(encodeStarted)
	}
	encoded := encodeTextFrame(frame)
	enqueueElapsed := time.Since(encodeStarted)
	// A takeover can happen between client selection and socket delivery. Retry
	// once against the fresh controller only when nobody received the frame.
	for attempt := 0; attempt < 2; attempt++ {
		admissionStarted := time.Now()
		h.mu.RLock()
		clients := make([]*client, 0, len(h.clients))
		for c := range h.clients {
			if h.isControllerClient(c) {
				clients = append(clients, c)
			}
		}
		h.mu.RUnlock()

		type pendingWrite struct {
			client  *client
			written chan error
		}
		pending := make([]pendingWrite, 0, len(clients))
		for _, c := range clients {
			written := make(chan error, 1)
			err := c.enqueueFrame(outboundFrame{payload: encoded, written: written, controllerOnly: true})
			if err != nil {
				frames, bytes := c.outboundSnapshot()
				h.logf("[wire] controller event dropped before write channel=%s queuedFrames=%d queuedBytes=%d", channel, frames, bytes)
				h.drop(c)
				continue
			}
			pending = append(pending, pendingWrite{client: c, written: written})
		}
		enqueueElapsed += time.Since(admissionStarted)

		delivered := 0
		for _, item := range pending {
			err := item.client.waitForWrite(item.written)
			if err == nil {
				delivered++
				continue
			}
			if !errors.Is(err, errControllerChanged) {
				h.drop(item.client)
			}
		}
		if delivered > 0 || len(clients) == 0 {
			return delivered, enqueueElapsed
		}
	}
	return 0, enqueueElapsed
}

func (h *Hub) broadcastWhere(channel string, payload any, include func(*client) bool) int {
	frame, err := json.Marshal(eventFrame{T: "event", Channel: channel, Payload: payload})
	if err != nil {
		return 0
	}
	encoded := encodeTextFrame(frame)
	h.recordChannel(&h.eventStats, channel, 0, len(encoded))

	h.mu.RLock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	delivered := 0
	for _, c := range clients {
		if !c.readySnapshot() {
			continue
		}
		if include != nil && !include(c) {
			continue
		}
		if err := c.enqueue(encoded); err != nil {
			frames, bytes := c.outboundSnapshot()
			h.logf("[wire] slow client dropped channel=%s queuedFrames=%d queuedBytes=%d", channel, frames, bytes)
			h.drop(c)
			continue
		}
		delivered++
	}
	return delivered
}

// HandleUpgrade performs the RFC 6455 server handshake and starts a read loop.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if rw.Reader.Buffered() > 0 {
		_ = conn.Close()
		return
	}
	accept := AcceptKey(key)
	_, err = io.WriteString(conn,
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")
	if err != nil {
		_ = conn.Close()
		return
	}

	c := h.newClient(conn, r)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	go h.writeLoop(c)
	h.announceClientAccess(c)
	if c.readySnapshot() {
		h.announceClientReady(c)
	}
	go h.readLoop(c)
}

func (h *Hub) announceClientReady(c *client) {
	h.mu.RLock()
	fn := h.onClientReady
	h.mu.RUnlock()
	if h.isControllerClient(c) {
		h.announceControllerReady(c)
	}
	if fn == nil {
		return
	}
	go fn(c.sendEvent)
}

func (h *Hub) announceControllerReady(c *client) {
	if !h.isControllerClient(c) {
		return
	}
	h.flushNotifyBacklog(c)
	h.mu.RLock()
	fn := h.onControllerReady
	h.mu.RUnlock()
	if fn != nil {
		go fn(func(channel string, payload any) error {
			return c.sendControllerEvent(channel, payload)
		})
	}
}

func (h *Hub) flushNotifyBacklog(c *client) {
	h.mu.Lock()
	if len(h.notifyBacklog) == 0 {
		h.mu.Unlock()
		return
	}
	items := append([]any(nil), h.notifyBacklog...)
	h.notifyBacklog = nil
	h.mu.Unlock()
	if err := c.sendControllerEventAndWait("notify:backlog", map[string]any{"items": items}); err != nil {
		h.QueueNotify(items...)
		if !errors.Is(err, errControllerChanged) {
			h.drop(c)
		}
	}
}

func (h *Hub) isControllerClient(c *client) bool {
	if c == nil || !c.readySnapshot() {
		return false
	}
	if h.lease == nil {
		return true
	}
	device, ok := c.deviceSnapshot()
	return ok && h.lease.IsController(device.ID)
}

// AcceptKey computes the Sec-WebSocket-Accept response header.
func AcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (h *Hub) newClient(conn net.Conn, r *http.Request) *client {
	c := &client{
		conn:       conn,
		outbound:   make(chan outboundFrame, outboundQueueFrameLimit),
		done:       make(chan struct{}),
		ip:         clientIP(r.RemoteAddr),
		deviceName: deviceNameFromRequest(r),
		userAgent:  strings.TrimSpace(r.UserAgent()),
	}
	if h.lease == nil {
		c.setAccessState(accessState{State: "approved"})
		return c
	}

	token := strings.TrimSpace(r.URL.Query().Get("deviceToken"))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token != "" {
		if device, ok := h.lease.AuthenticateToken(token, c.ip); ok {
			c.setDevice(device, "")
			return c
		}
		c.setAccessState(accessState{State: "rejected", Reason: "invalid-token"})
		return c
	}

	if h.trustLocalhost && isLocalIP(c.ip) {
		device, issuedToken, err := h.lease.ApproveDevice(c.deviceName, c.ip)
		if err != nil {
			h.logf("[lan] localhost auto-pair failed: %v", err)
			c.setAccessState(accessState{State: "rejected", Reason: "auto-approve-failed"})
			return c
		}
		c.setDevice(device, issuedToken)
		return c
	}

	requestID, requestedAt := h.addPendingAccess(c)
	c.setPendingAccess(requestID, requestedAt)
	return c
}

func (h *Hub) addPendingAccess(c *client) (string, time.Time) {
	now := time.Now().UTC()
	h.mu.Lock()
	h.requestSeq++
	requestID := fmt.Sprintf("lan-%d-%d", now.UnixMilli(), h.requestSeq)
	rec := &pendingAccess{
		requestID:   requestID,
		ip:          c.ip,
		deviceName:  c.deviceName,
		userAgent:   c.userAgent,
		requestedAt: now,
		client:      c,
	}
	rec.timer = time.AfterFunc(h.accessTimeout, func() {
		h.denyPendingAccess(requestID, "timeout")
	})
	h.pending[requestID] = rec
	h.mu.Unlock()
	return requestID, now
}

func (h *Hub) announceClientAccess(c *client) {
	if h.lease == nil {
		return
	}
	if device, ok := c.deviceSnapshot(); ok {
		h.ensureController(device)
		_ = c.sendEvent("lan:access-state", h.accessApprovedPayload(c, device))
		return
	}
	state := c.accessSnapshot()
	switch state.State {
	case "waiting":
		_ = c.sendEvent("lan:access-state", h.accessStatePayload(state))
		h.Broadcast("lan:access-request", map[string]any{
			"requestId":   state.RequestID,
			"ip":          c.ip,
			"deviceName":  c.deviceName,
			"userAgent":   c.userAgent,
			"requestedAt": state.RequestedAt,
		})
	case "rejected":
		_ = c.sendEventAndWait("lan:access-state", h.accessStatePayload(state))
		h.drop(c)
	}
}

func (h *Hub) readLoop(c *client) {
	defer h.drop(c)

	var buf []byte
	dec := &frameDecoder{}
	tmp := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			messages, rest, closeFrame := dec.drain(buf)
			buf = rest
			for _, raw := range messages {
				h.handleRaw(c, raw)
			}
			if closeFrame {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *Hub) handleRaw(c *client, raw []byte) {
	msg, ok := decodeInvoke(raw)
	if !ok {
		return
	}

	var result any
	var errText *string
	result, err := h.invokeForClient(c, msg.Channel, msg.Args)
	if err != nil {
		s := err.Error()
		errText = &s
		result = nil
	}
	var frame []byte
	replyBytes := 0
	if rawResult, ok := result.(RawResult); ok && errText == nil {
		var rawErr error
		frame, replyBytes, rawErr = encodeRawResultReplyFrame(msg.ID, rawResult)
		if rawErr != nil {
			s := rawErr.Error()
			reply, _ := json.Marshal(replyFrame{T: "reply", ID: msg.ID, Result: nil, Error: &s})
			replyBytes = len(reply)
			frame = encodeTextFrame(reply)
		}
	} else {
		reply, marshalErr := json.Marshal(replyFrame{T: "reply", ID: msg.ID, Result: result, Error: errText})
		if marshalErr != nil {
			s := marshalErr.Error()
			reply, _ = json.Marshal(replyFrame{T: "reply", ID: msg.ID, Result: nil, Error: &s})
		}
		replyBytes = len(reply)
		frame = encodeTextFrame(reply)
	}
	if err := c.enqueueAndWait(frame); err != nil {
		h.logf("[wire] invoke reply could not be written channel=%s replyBytes=%d err=%v", msg.Channel, replyBytes, err)
		h.drop(c)
	}
	// An implicit handover announces itself exactly like an explicit one, so the
	// device that lost the lease learns it from the daemon rather than from its
	// next action failing.
	moved := c.tookControl.Swap(false)
	if h.lease != nil && (moved || (msg.Channel == "lan:take-control" && errText == nil)) {
		if device, ok := c.deviceSnapshot(); ok {
			h.Broadcast("lan:controller-changed", controllerPayload(device))
			h.announceControllerReady(c)
		}
	}
}

func encodeRawResultReplyFrame(id any, raw RawResult) ([]byte, int, error) {
	if !json.Valid(raw) {
		return nil, 0, errors.New("wire raw result is not valid JSON")
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, 0, err
	}
	prefix := make([]byte, 0, len(idJSON)+32)
	prefix = append(prefix, `{"t":"reply","id":`...)
	prefix = append(prefix, idJSON...)
	prefix = append(prefix, `,"result":`...)
	suffix := []byte(`,"error":null}`)
	replyLen := len(prefix) + len(raw) + len(suffix)
	headerLen := textFrameHeaderLen(replyLen)
	frameLen := headerLen + replyLen

	var frame []byte
	if cap(raw) >= frameLen {
		frame = raw[:frameLen]
		copy(frame[headerLen+len(prefix):], raw)
	} else {
		frame = make([]byte, frameLen)
		copy(frame[headerLen+len(prefix):], raw)
	}
	writeTextFrameHeader(frame[:headerLen], replyLen)
	copy(frame[headerLen:], prefix)
	copy(frame[headerLen+len(prefix)+len(raw):], suffix)
	return frame, replyLen, nil
}

func (h *Hub) invokeForClient(c *client, channel string, args []any) (any, error) {
	if h.lease == nil {
		return h.Invoke(channel, args)
	}
	if channel == "lan:pairing-info" {
		return h.handlePairingInfo(c, args)
	}
	// The two channels a parked client may call. Enrolment is how a client that
	// already holds the fleet key stops being parked without a human deciding
	// anything, which is what makes one chat list across many machines possible.
	if channel == "fleet:challenge" {
		return h.handleFleetChallenge(c)
	}
	if channel == "fleet:enroll" {
		return h.handleFleetEnroll(c, args)
	}

	device, paired := c.deviceSnapshot()
	if !paired {
		state := c.accessSnapshot()
		if state.State == "waiting" {
			return nil, structuredError("lan:access-pending", "waiting for approval", map[string]any{
				"requestId": state.RequestID,
				"state":     state.State,
			})
		}
		return nil, structuredError("lan:access-rejected", "access rejected", map[string]any{
			"state":  firstNonEmpty(state.State, "rejected"),
			"reason": state.Reason,
		})
	}
	h.touchClientDevice(c)
	h.ensureController(device)
	if channel == "lan:take-control" {
		return h.handleTakeControl(c)
	}
	if channel == "lan:devices" {
		return h.handleDevices(), nil
	}
	if channel == "lan:access-decide" {
		if !h.lease.IsController(device.ID) {
			return nil, h.notControllerError(device)
		}
		return h.handleAccessDecide(args)
	}
	if channel == "lan:revoke" {
		if !h.lease.IsController(device.ID) {
			return nil, h.notControllerError(device)
		}
		return h.handleRevoke(args)
	}
	if _, admin := fleetAdminChannels[channel]; admin {
		if !h.lease.IsController(device.ID) {
			return nil, h.notControllerError(device)
		}
		return h.handleFleetAdmin(c, channel, args)
	}
	if _, mutating := mutatingChannels[channel]; mutating && !h.lease.IsController(device.ID) {
		// One human, several devices (no accounts by design). An explicit "take
		// control" ceremony buys no safety here, because you cannot type on two
		// devices at once — it only makes the phone in your hand refuse to send
		// the message you are looking at. Acting takes the lease instead of being
		// refused it. The invariant the lease exists for is untouched: exactly one
		// device owns a decision at any instant, so two of them can never answer
		// the same permission card.
		//
		// Admitting or revoking a DEVICE is handled above and stays explicit. That
		// one genuinely needs the human at that screen, and a stolen lease there
		// would let a device let another device in.
		if h.lease.TakeControl(device) {
			c.tookControl.Store(true)
		}
	}
	return h.Invoke(channel, args)
}

func (h *Hub) handlePairingInfo(c *client, args []any) (any, error) {
	payload := map[string]any{"deprecated": true}
	if device, paired := c.deviceSnapshot(); paired {
		payload["access"] = h.accessApprovedPayload(c, device)
		return payload, nil
	}
	state := c.accessSnapshot()
	if state.State != "" {
		payload["access"] = accessStatePayload(state)
	}
	return payload, nil
}

// SetFleet installs the fleet key store and this machine's id. It is a setter
// rather than an Option because a hub exists before the daemon has loaded its
// identity, and the proof is bound to that id.
func (h *Hub) SetFleet(store *fleet.Store, machineID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fleetKeys = store
	h.machineID = strings.TrimSpace(machineID)
}

func (h *Hub) fleetSnapshot() (*fleet.Store, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fleetKeys, h.machineID
}

// handleFleetChallenge hands a parked client the freshness it needs to prove it
// holds the fleet key. The server nonce is the whole reason for a round trip:
// without it, whoever captured one proof could replay it as their own.
func (h *Hub) handleFleetChallenge(c *client) (any, error) {
	store, machineID := h.fleetSnapshot()
	if store == nil || !store.Has() {
		// Not an error. A daemon with no key is one nobody has joined to a fleet
		// yet, and a client should be able to say exactly that.
		return map[string]any{"enabled": false, "machineId": machineID}, nil
	}
	nonce, err := fleet.NewNonce()
	if err != nil {
		return nil, err
	}
	c.setFleetNonce(nonce)
	payload := map[string]any{
		"enabled":     true,
		"machineId":   machineID,
		"serverNonce": nonce,
		"keyIds":      store.KeyIDs(),
		"nonceTtlMs":  fleetChallengeTTL.Milliseconds(),
	}
	h.mu.RLock()
	fingerprint := h.tlsFingerprint
	h.mu.RUnlock()
	if fingerprint != "" {
		// One proof per key this daemon holds, so a client can check the
		// certificate under whichever key it happens to have.
		proofs := make(map[string]string, len(store.Keys()))
		for _, key := range store.Keys() {
			proofs[key.KeyID] = fleet.CertProof(key.Secret, nonce, fingerprint)
		}
		payload["certFingerprint"] = fingerprint
		payload["certProofs"] = proofs
	}
	return payload, nil
}

// handleFleetEnroll converts a parked client into an approved device when it can
// prove it holds a key this daemon also holds.
//
// The derived token is deliberately absent from the reply: the client computed
// the same value from the same nonces, so nothing reusable is ever sent. That is
// what makes this safe to run before TLS exists.
func (h *Hub) handleFleetEnroll(c *client, args []any) (any, error) {
	if device, paired := c.deviceSnapshot(); paired {
		// Idempotent: a retry after a lost reply must not mint a second device.
		return map[string]any{"ok": true, "deviceId": device.ID, "name": device.Name, "alreadyEnrolled": true}, nil
	}
	store, machineID := h.fleetSnapshot()
	if store == nil || !store.Has() {
		return nil, structuredError("fleet:no-key", "this machine has no fleet key yet", nil)
	}
	if c.fleetLocked() {
		return nil, structuredError("fleet:locked", "too many failed attempts on this connection; reconnect to try again", nil)
	}
	arg := firstMapArg(args)
	clientNonce := stringField(arg, "clientNonce")
	proof := stringField(arg, "proof")
	if clientNonce == "" || proof == "" {
		return nil, structuredError("fleet:malformed", "clientNonce and proof are required", nil)
	}
	serverNonce, issuedAt, ok := c.fleetNonceSnapshot()
	if !ok || time.Since(issuedAt) > fleetChallengeTTL {
		return nil, structuredError("fleet:challenge-expired", "ask for a fresh challenge first", nil)
	}
	key, token, verified := store.Verify(serverNonce, clientNonce, machineID, proof)
	if !verified {
		// One nonce, one attempt: a failed proof must never leave a live
		// challenge behind for the next guess to reuse.
		failures := c.failFleetAttempt()
		h.logf("[fleet] enrolment refused for %s (attempt %d)", c.ip, failures)
		return nil, structuredError("fleet:rejected", "that key does not belong to this fleet", nil)
	}
	device, err := h.lease.EnrollDevice(firstNonEmpty(stringField(arg, "name"), c.deviceName), c.ip, token, key.Owner, key.KeyID)
	if err != nil {
		return nil, err
	}
	c.clearFleetNonce()
	// Withdraw the pending-access request this client arrived with, or its timer
	// will deny a device that is already approved.
	h.removePendingForClient(c)
	c.setDevice(device, "")
	h.ensureController(device)
	_ = c.sendEvent("lan:access-state", h.accessApprovedPayload(c, device))
	h.announceClientReady(c)
	// Enrolment is silent by design, so it has to be loud in the record: a key
	// used by someone else shows up here and nowhere else.
	h.Broadcast("fleet:enrolled", map[string]any{
		"deviceId":  device.ID,
		"name":      device.Name,
		"ip":        c.ip,
		"userAgent": c.userAgent,
		"keyId":     key.KeyID,
		"owner":     key.Owner,
		"machineId": machineID,
		"at":        time.Now().UTC().Format(time.RFC3339),
	})
	h.logf("[fleet] %s enrolled %q under key %s", c.ip, device.Name, key.KeyID)
	return map[string]any{
		"ok": true, "deviceId": device.ID, "name": device.Name,
		"keyId": key.KeyID, "owner": key.Owner, "machineId": machineID,
	}, nil
}

func (h *Hub) handleTakeControl(c *client) (any, error) {
	device, paired := c.deviceSnapshot()
	if !paired {
		return nil, structuredError("lan:access-rejected", "access rejected", nil)
	}
	changed := h.lease.TakeControl(device)
	return map[string]any{
		"ok":         true,
		"changed":    changed,
		"deviceId":   device.ID,
		"name":       device.Name,
		"controller": true,
	}, nil
}

func (h *Hub) handleAccessDecide(args []any) (any, error) {
	arg := firstMapArg(args)
	requestID := strings.TrimSpace(fmt.Sprint(arg["requestId"]))
	allow, _ := boolField(arg, "allow")
	if requestID == "" {
		return map[string]any{"ok": false, "error": "missing requestId"}, nil
	}
	if !allow {
		ok := h.denyPendingAccess(requestID, "denied")
		return map[string]any{"ok": ok, "allowed": false}, nil
	}
	rec := h.takePendingAccess(requestID)
	if rec == nil {
		return map[string]any{"ok": false, "error": "not found"}, nil
	}
	device, token, err := h.lease.ApproveDevice(rec.deviceName, rec.ip)
	if err != nil {
		return nil, err
	}
	rec.client.setDevice(device, token)
	h.ensureController(device)
	_ = rec.client.sendEvent("lan:access-state", h.accessApprovedPayload(rec.client, device))
	h.announceClientReady(rec.client)
	return map[string]any{"ok": true, "allowed": true, "deviceId": device.ID, "name": device.Name}, nil
}

func (h *Hub) handleDevices() map[string]any {
	h.refreshConnectedDevices()
	devices := h.lease.Devices()
	out := make([]any, 0, len(devices))
	for _, device := range devices {
		out = append(out, map[string]any{
			"deviceId":   device.ID,
			"name":       device.Name,
			"ip":         device.IP,
			"lastSeen":   device.LastSeen,
			"controller": h.lease.IsController(device.ID),
		})
	}
	return map[string]any{"devices": out}
}

func (h *Hub) deviceRefreshLoop() {
	ticker := time.NewTicker(h.deviceRefresh)
	defer ticker.Stop()
	for range ticker.C {
		h.refreshConnectedDevices()
	}
}

func (h *Hub) refreshConnectedDevices() {
	if h.lease == nil {
		return
	}
	h.mu.RLock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		h.touchClientDevice(c)
	}
}

func (h *Hub) touchClientDevice(c *client) {
	if h.lease == nil || c == nil {
		return
	}
	device, ok := c.deviceSnapshot()
	if !ok {
		return
	}
	if h.lease.TouchDevice(device.ID, c.ip) {
		c.setDeviceNoToken(device.ID, c.ip)
	}
}

func (h *Hub) handleRevoke(args []any) (any, error) {
	arg := firstMapArg(args)
	deviceID := strings.TrimSpace(fmt.Sprint(arg["deviceId"]))
	if deviceID == "" {
		deviceID = stringArg(args, 0)
	}
	if deviceID == "" {
		return map[string]any{"ok": false, "error": "missing deviceId"}, nil
	}
	return map[string]any{"ok": h.lease.RevokeDevice(deviceID)}, nil
}

// accessStatePayload is the hub's stamped form: the same document plus who is
// answering. Every socket receives exactly one of these the moment it opens, in
// all three outcomes, which makes it the handshake payload a non-browser client
// can read without a DOM.
func (h *Hub) accessStatePayload(state accessState) map[string]any {
	return h.stampIdentity(accessStatePayload(state))
}

// stampIdentity names the daemon on the socket itself.
//
// A client owns its own reconnect generation — it is a counter in the client,
// incremented per connect attempt, and the daemon neither sees nor sends it. But
// a counter cannot tell "reconnected to the same daemon" from "reconnected to a
// daemon that restarted underneath me", and those need different recoveries: the
// second one lost every engine and in-memory session. instanceId is minted once
// per process, so a change in it across two connects is a restart, said plainly
// instead of inferred.
func (h *Hub) stampIdentity(payload map[string]any) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["instanceId"] = h.instanceID
	h.mu.RLock()
	machineID := h.machineID
	h.mu.RUnlock()
	if machineID != "" {
		payload["machineId"] = machineID
	}
	return payload
}

func (h *Hub) accessApprovedPayload(c *client, device lease.Device) map[string]any {
	payload := map[string]any{
		"state":       "approved",
		"deviceId":    device.ID,
		"name":        device.Name,
		"controller":  h.lease.IsController(device.ID),
		"deviceToken": nil,
	}
	if token := c.consumeIssuedToken(); token != "" {
		payload["deviceToken"] = token
	}
	return h.stampIdentity(payload)
}

func (h *Hub) notControllerError(device lease.Device) error {
	fields := map[string]any{"deviceId": device.ID}
	if controller, ok := h.lease.Controller(); ok {
		fields["controllerDeviceId"] = controller.ID
		fields["controllerName"] = controller.Name
	}
	return structuredError("lan:not-controller", "controller lease required", fields)
}

func (h *Hub) ensureController(device lease.Device) bool {
	if h.lease == nil {
		return false
	}
	return h.lease.EnsureController(device)
}

func (h *Hub) takePendingAccess(requestID string) *pendingAccess {
	h.mu.Lock()
	rec := h.pending[requestID]
	if rec == nil {
		h.mu.Unlock()
		return nil
	}
	delete(h.pending, requestID)
	if rec.timer != nil {
		rec.timer.Stop()
	}
	h.mu.Unlock()
	rec.client.clearPendingAccess(requestID)
	return rec
}

func (h *Hub) denyPendingAccess(requestID, reason string) bool {
	rec := h.takePendingAccess(requestID)
	if rec == nil {
		return false
	}
	rec.client.setAccessState(accessState{
		State:       reason,
		RequestID:   requestID,
		Reason:      reason,
		RequestedAt: rec.requestedAt.Format(time.RFC3339),
	})
	_ = rec.client.sendEventAndWait("lan:access-state", h.accessStatePayload(rec.client.accessSnapshot()))
	h.drop(rec.client)
	return true
}

func (h *Hub) removePendingForClient(c *client) {
	requestID := c.pendingRequestIDSnapshot()
	if requestID == "" {
		return
	}
	h.mu.Lock()
	rec := h.pending[requestID]
	if rec != nil && rec.client == c {
		delete(h.pending, requestID)
		if rec.timer != nil {
			rec.timer.Stop()
		}
	}
	h.mu.Unlock()
	c.clearPendingAccess(requestID)
}

func (h *Hub) drop(c *client) {
	if c == nil {
		return
	}
	// A dropped controller is the loudest possible symptom: it forces a full
	// rehydrate. Counting drops makes the hydrate→drop→reconnect loop visible
	// without reading logs.
	atomic.AddUint64(&h.stats.drops, 1)
	h.removePendingForClient(c)
	device, paired := c.deviceSnapshot()
	h.mu.Lock()
	_, connected := h.clients[c]
	if connected {
		delete(h.clients, c)
	}
	deviceStillConnected := false
	if connected && paired && h.lease != nil {
		for other := range h.clients {
			otherDevice, ok := other.deviceSnapshot()
			if ok && otherDevice.ID == device.ID {
				deviceStillConnected = true
				break
			}
		}
		if !deviceStillConnected {
			h.lease.ReleaseController(device.ID)
		}
	}
	h.mu.Unlock()
	if connected {
		c.close()
	}
}

func (h *Hub) writeLoop(c *client) {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		select {
		case <-c.done:
			return
		case frame := <-c.outbound:
			var err error
			if frame.controllerOnly && !h.isControllerClient(c) {
				err = errControllerChanged
			} else {
				_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeoutForPayload(len(frame.payload))))
				err = writeAll(c.conn, frame.payload)
			}
			c.outMu.Lock()
			c.outboundBytes -= len(frame.payload)
			if outboundFrameIsBulk(len(frame.payload)) {
				c.outboundBulkBytes -= len(frame.payload)
			}
			c.outMu.Unlock()
			if frame.written != nil {
				frame.written <- err
			}
			if err != nil && !errors.Is(err, errControllerChanged) {
				frames, bytes := c.outboundSnapshot()
				h.logf("[wire] client socket write failed queuedFrames=%d queuedBytes=%d", frames, bytes)
				h.drop(c)
				return
			}
		}
	}
}

// enqueue preserves per-client wire order without letting a slow or stale
// connection delay healthy clients. A client that falls more than a bounded
// backlog behind is disconnected and can recover from the authoritative
// session snapshot on reconnect. A bulk frame (larger than the regular byte
// budget — session:get in real profiles) is admitted one at a time alongside
// that budget rather than against it: charging it to the shared budget made a
// draining snapshot and live events evict each other, which is how a slow
// client could never finish hydrating. No individual frame may exceed
// outboundFrameByteLimit.
func (c *client) enqueue(frame []byte) error {
	return c.enqueueFrame(outboundFrame{payload: frame})
}

func (c *client) enqueueFrame(frame outboundFrame) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	select {
	case <-c.done:
		return errClientClosed
	default:
	}
	if len(frame.payload) > outboundFrameByteLimit {
		return errOutboundFrameTooLarge
	}
	if len(c.outbound) >= outboundQueueFrameLimit {
		return errOutboundQueueFull
	}
	if outboundFrameIsBulk(len(frame.payload)) {
		if c.outboundBulkBytes > 0 {
			return errOutboundQueueFull
		}
	} else if regular := c.outboundBytes - c.outboundBulkBytes; regular+len(frame.payload) > outboundQueueByteLimit {
		return errOutboundQueueFull
	}
	select {
	case c.outbound <- frame:
		c.outboundBytes += len(frame.payload)
		if outboundFrameIsBulk(len(frame.payload)) {
			c.outboundBulkBytes += len(frame.payload)
		}
		return nil
	default:
		return errOutboundQueueFull
	}
}

func (c *client) outboundSnapshot() (frames int, bytes int) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return len(c.outbound), c.outboundBytes
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *client) setDevice(device lease.Device, issuedToken string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.device = &device
	c.issuedToken = issuedToken
	c.pendingRequestID = ""
	c.access = accessState{State: "approved"}
}

func (c *client) setFleetNonce(nonce string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.fleetNonce = nonce
	c.fleetNonceAt = time.Now()
}

func (c *client) fleetNonceSnapshot() (string, time.Time, bool) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.fleetNonce, c.fleetNonceAt, c.fleetNonce != ""
}

func (c *client) clearFleetNonce() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.fleetNonce = ""
}

// failFleetAttempt burns the challenge along with the attempt and counts it.
func (c *client) failFleetAttempt() int {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.fleetNonce = ""
	c.fleetFailures++
	return c.fleetFailures
}

func (c *client) fleetLocked() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.fleetFailures >= fleetMaxFailures
}

func (c *client) setDeviceNoToken(deviceID, ip string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.device == nil || c.device.ID != deviceID {
		return
	}
	if strings.TrimSpace(ip) != "" {
		c.device.IP = strings.TrimSpace(ip)
	}
}

func (c *client) deviceSnapshot() (lease.Device, bool) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.device == nil || c.device.ID == "" {
		return lease.Device{}, false
	}
	return *c.device, true
}

func (c *client) setPendingAccess(requestID string, requestedAt time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.pendingRequestID = requestID
	c.access = accessState{
		State:       "waiting",
		RequestID:   requestID,
		RequestedAt: requestedAt.Format(time.RFC3339),
	}
}

func (c *client) clearPendingAccess(requestID string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.pendingRequestID == requestID {
		c.pendingRequestID = ""
	}
}

func (c *client) pendingRequestIDSnapshot() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.pendingRequestID
}

func (c *client) setAccessState(state accessState) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.access = state
}

func (c *client) accessSnapshot() accessState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.access
}

func (c *client) readySnapshot() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.access.State == "approved"
}

func (c *client) consumeIssuedToken() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	token := c.issuedToken
	c.issuedToken = ""
	return token
}

func (c *client) sendEvent(channel string, payload any) error {
	return c.sendEventWithScope(channel, payload, false, false)
}

func (c *client) sendControllerEvent(channel string, payload any) error {
	return c.sendEventWithScope(channel, payload, true, false)
}

func (c *client) sendEventAndWait(channel string, payload any) error {
	return c.sendEventWithScope(channel, payload, false, true)
}

func (c *client) sendControllerEventAndWait(channel string, payload any) error {
	return c.sendEventWithScope(channel, payload, true, true)
}

func (c *client) sendEventWithScope(channel string, payload any, controllerOnly, wait bool) error {
	frame, err := json.Marshal(eventFrame{T: "event", Channel: channel, Payload: payload})
	if err != nil {
		return err
	}
	encoded := encodeTextFrame(frame)
	if !wait {
		return c.enqueueFrame(outboundFrame{payload: encoded, controllerOnly: controllerOnly})
	}
	written := make(chan error, 1)
	if err := c.enqueueFrame(outboundFrame{payload: encoded, written: written, controllerOnly: controllerOnly}); err != nil {
		return err
	}
	return c.waitForWrite(written)
}

func (c *client) enqueueAndWait(frame []byte) error {
	written := make(chan error, 1)
	if err := c.enqueueFrame(outboundFrame{payload: frame, written: written}); err != nil {
		return err
	}
	return c.waitForWrite(written)
}

func (c *client) waitForWrite(written <-chan error) error {
	select {
	case err := <-written:
		return err
	case <-c.done:
		select {
		case err := <-written:
			return err
		default:
			return errClientClosed
		}
	}
}

type invokeMessage struct {
	ID      any
	Channel string
	Args    []any
}

type replyFrame struct {
	T      string  `json:"t"`
	ID     any     `json:"id"`
	Result any     `json:"result"`
	Error  *string `json:"error"`
}

type eventFrame struct {
	T       string `json:"t"`
	Channel string `json:"channel"`
	Payload any    `json:"payload"`
}

func decodeInvoke(raw []byte) (invokeMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return invokeMessage{}, false
	}

	var typ string
	if err := json.Unmarshal(fields["t"], &typ); err != nil || typ != "invoke" {
		return invokeMessage{}, false
	}
	var channel string
	if err := json.Unmarshal(fields["channel"], &channel); err != nil {
		channel = ""
	}
	var id any
	if rawID, ok := fields["id"]; ok {
		id = decodeJSONAny(rawID)
	}
	args := decodeArgs(fields["args"])
	return invokeMessage{ID: id, Channel: channel, Args: args}, true
}

func decodeArgs(raw json.RawMessage) []any {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []any{}
	}
	var args []any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		return []any{}
	}
	return args
}

func decodeJSONAny(raw json.RawMessage) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

func firstMapArg(args []any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	m, _ := args[0].(map[string]any)
	return m
}

// stringField reads a string field without inventing one. fmt.Sprint would turn
// a missing key into "<nil>", which passes an emptiness check.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

func stringArg(args []any, idx int) string {
	if idx < 0 || idx >= len(args) {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(args[idx]))
}

func boolField(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	switch v := m[key].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "allow", "allowed":
			return true, true
		case "false", "0", "no", "deny", "denied":
			return false, true
		}
	}
	return false, false
}

func accessStatePayload(state accessState) map[string]any {
	payload := map[string]any{"state": state.State}
	if state.RequestID != "" {
		payload["requestId"] = state.RequestID
	}
	if state.Reason != "" {
		payload["reason"] = state.Reason
	}
	if state.RequestedAt != "" {
		payload["requestedAt"] = state.RequestedAt
	}
	return payload
}

func structuredError(code, message string, fields map[string]any) error {
	payload := map[string]any{
		"code":    code,
		"message": message,
	}
	for k, v := range fields {
		payload[k] = v
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New(message)
	}
	return errors.New(string(data))
}

func controllerPayload(device lease.Device) map[string]any {
	return map[string]any{"deviceId": device.ID, "name": device.Name}
}

func deviceNameFromRequest(r *http.Request) string {
	name := strings.TrimSpace(r.URL.Query().Get("deviceName"))
	if name == "" {
		name = strings.TrimSpace(r.Header.Get("X-Device-Name"))
	}
	if name == "" {
		name = strings.TrimSpace(r.UserAgent())
	}
	if name == "" {
		name = "Unknown device"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if strings.HasPrefix(host, "::ffff:") {
		host = strings.TrimPrefix(host, "::ffff:")
	}
	return host
}

func isLocalIP(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || ip == "localhost" || ip == ""
}

func encodeTextFrame(payload []byte) []byte {
	headerLen := textFrameHeaderLen(len(payload))
	out := make([]byte, headerLen+len(payload))
	writeTextFrameHeader(out[:headerLen], len(payload))
	copy(out[headerLen:], payload)
	return out
}

func textFrameHeaderLen(payloadLen int) int {
	switch {
	case payloadLen < 126:
		return 2
	case payloadLen <= 0xffff:
		return 4
	default:
		return 10
	}
}

func writeTextFrameHeader(header []byte, payloadLen int) {
	header[0] = 0x81
	switch len(header) {
	case 2:
		header[1] = byte(payloadLen)
	case 4:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(payloadLen))
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(payloadLen))
	}
}

type frameDecoder struct {
	fragment []byte
}

func (d *frameDecoder) drain(buf []byte) ([][]byte, []byte, bool) {
	var messages [][]byte
	closeFrame := false
	off := 0

	for off+2 <= len(buf) {
		b0 := buf[off]
		b1 := buf[off+1]
		fin := b0&0x80 != 0
		opcode := b0 & 0x0f
		masked := b1&0x80 != 0
		length := uint64(b1 & 0x7f)
		p := off + 2

		switch length {
		case 126:
			if p+2 > len(buf) {
				return messages, buf[off:], closeFrame
			}
			length = uint64(binary.BigEndian.Uint16(buf[p : p+2]))
			p += 2
		case 127:
			if p+8 > len(buf) {
				return messages, buf[off:], closeFrame
			}
			length = binary.BigEndian.Uint64(buf[p : p+8])
			p += 8
		}
		if length > uint64(len(buf)-p) {
			return messages, buf[off:], closeFrame
		}

		var mask []byte
		if masked {
			if p+4 > len(buf) {
				return messages, buf[off:], closeFrame
			}
			mask = buf[p : p+4]
			p += 4
			if length > uint64(len(buf)-p) {
				return messages, buf[off:], closeFrame
			}
		}

		payloadLen := int(length)
		payload := buf[p : p+payloadLen]
		if masked {
			unmasked := make([]byte, payloadLen)
			for i := range unmasked {
				unmasked[i] = payload[i] ^ mask[i&3]
			}
			payload = unmasked
		}
		off = p + payloadLen

		switch opcode {
		case 0x8:
			closeFrame = true
			return messages, buf[off:], closeFrame
		case 0x9, 0xA:
			continue
		case 0x1:
			if fin {
				messages = append(messages, append([]byte(nil), payload...))
				d.fragment = nil
			} else {
				d.fragment = append(d.fragment[:0], payload...)
			}
		case 0x0:
			d.fragment = append(d.fragment, payload...)
			if fin {
				messages = append(messages, append([]byte(nil), d.fragment...))
				d.fragment = nil
			}
		}
	}

	return messages, buf[off:], closeFrame
}

var errShortWrite = errors.New("short websocket write")

func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return errShortWrite
	}
	return nil
}
