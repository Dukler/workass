package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/lease"
)

func TestAccessApprovalFlowIssuesTokenAndReconnectWorks(t *testing.T) {
	manager, _, stateDir := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.Register("state:get", func(args []any) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=controller", "YXBwcm92ZS1jdHJs")
	defer controller.conn.Close()
	approved := readEvent(t, controller)
	if approved.Channel != "lan:access-state" || approved.Payload["state"] != "approved" || approved.Payload["controller"] != true {
		t.Fatalf("controller access event = %+v", approved)
	}

	pending := dialWSPath(t, server.URL, "/?deviceName=phone", "YXBwcm92ZS1waG9uZQ==")
	defer pending.conn.Close()
	waiting := readEvent(t, pending)
	if waiting.Channel != "lan:access-state" || waiting.Payload["state"] != "waiting" || waiting.Payload["requestId"] == "" {
		t.Fatalf("pending access event = %+v", waiting)
	}
	request := readEvent(t, controller)
	if request.Channel != "lan:access-request" || request.Payload["requestId"] != waiting.Payload["requestId"] || request.Payload["deviceName"] != "phone" {
		t.Fatalf("access request event = %+v waiting=%+v", request, waiting)
	}

	sendInvoke(t, controller, 1, "lan:access-decide", map[string]any{"requestId": request.Payload["requestId"], "allow": true})
	decide := readReply(t, controller)
	if decide.Error != nil || decide.Result.(map[string]any)["allowed"] != true {
		t.Fatalf("access decide reply = %+v", decide)
	}
	issued := readEvent(t, pending)
	if issued.Channel != "lan:access-state" || issued.Payload["state"] != "approved" {
		t.Fatalf("issued access event channel=%s state=%v", issued.Channel, issued.Payload["state"])
	}
	token, _ := issued.Payload["deviceToken"].(string)
	deviceID, _ := issued.Payload["deviceId"].(string)
	if len(token) != 64 || deviceID == "" {
		t.Fatalf("approved device missing token/id tokenLen=%d deviceIDEmpty=%v", len(token), deviceID == "")
	}
	state, err := os.ReadFile(filepath.Join(stateDir, "devices.json"))
	if err != nil {
		t.Fatalf("read devices state: %v", err)
	}
	if bytes.Contains(state, []byte(token)) || !bytes.Contains(state, []byte(`"ip"`)) || !bytes.Contains(state, []byte(`"approvedAt"`)) {
		t.Fatalf("device state missing approval metadata or leaked plaintext token")
	}

	reconnected := dialWSPath(t, server.URL, "/?deviceToken="+token+"&deviceName=phone", "YXBwcm92ZS1yZWNvbm4=")
	defer reconnected.conn.Close()
	reconnectState := readEvent(t, reconnected)
	if reconnectState.Channel != "lan:access-state" || reconnectState.Payload["state"] != "approved" {
		t.Fatalf("reconnect access state = %+v", reconnectState)
	}
	sendInvoke(t, reconnected, 2, "state:get")
	stateReply := readReply(t, reconnected)
	if stateReply.Error != nil || stateReply.Result.(map[string]any)["ok"] != true {
		t.Fatalf("state:get after reconnect = %+v", stateReply)
	}
}

func TestLanDevicesRefreshesLastSeenAndConnectedIP(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	setNow := func(ti time.Time) {
		mu.Lock()
		defer mu.Unlock()
		now = ti
	}
	manager, err := lease.NewManager(lease.Options{
		StateDir: t.TempDir(),
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}
	device, token, err := manager.ApproveDevice("controller", "10.0.0.10")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false, DeviceRefreshInterval: 10 * time.Millisecond})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWSPath(t, server.URL, "/?deviceToken="+token+"&deviceName=controller", "ZGV2aWNlcy1yZWZyZXNo")
	defer client.conn.Close()
	_ = readEvent(t, client)

	invokeSeen := time.Date(2026, 7, 10, 12, 0, 1, 0, time.UTC)
	setNow(invokeSeen)
	sendInvoke(t, client, 1, "lan:devices")
	reply := readReply(t, client)
	if reply.Error != nil {
		t.Fatalf("lan:devices reply error = %+v", reply)
	}
	devices := reply.Result.(map[string]any)["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices reply = %#v", reply.Result)
	}
	got := devices[0].(map[string]any)
	if got["deviceId"] != device.ID || got["ip"] != "127.0.0.1" || got["lastSeen"] != invokeSeen.Format(time.RFC3339) || got["controller"] != true {
		t.Fatalf("lan:devices refreshed payload = %#v", got)
	}

	periodicSeen := time.Date(2026, 7, 10, 12, 0, 2, 0, time.UTC)
	setNow(periodicSeen)
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		devs := manager.Devices()
		if len(devs) == 1 && devs[0].LastSeen == periodicSeen.Format(time.RFC3339) && devs[0].IP == "127.0.0.1" {
			t.Logf("trace lan:devices refreshed invoke=%s periodic=%s ip=%s", got["lastSeen"], devs[0].LastSeen, devs[0].IP)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic lastSeen did not refresh; devices=%#v", devs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAccessDenyClosesPendingClient(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "ZGVueS1jdHJs")
	defer controller.conn.Close()
	_ = readEvent(t, controller)
	pending := dialWSPath(t, server.URL, "/?deviceName=tablet", "ZGVueS1wZW5kaW5n")
	defer pending.conn.Close()
	waiting := readEvent(t, pending)
	request := readEvent(t, controller)

	sendInvoke(t, controller, 1, "lan:access-decide", map[string]any{"requestId": request.Payload["requestId"], "allow": false})
	decide := readReply(t, controller)
	if decide.Error != nil || decide.Result.(map[string]any)["allowed"] != false {
		t.Fatalf("deny reply = %+v", decide)
	}
	denied := readEvent(t, pending)
	if denied.Channel != "lan:access-state" || denied.Payload["state"] != "denied" || denied.Payload["requestId"] != waiting.Payload["requestId"] {
		t.Fatalf("denied event = %+v", denied)
	}
}

func TestAccessRequestTimeoutDeniesPendingClient(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false, AccessRequestTimeout: 20 * time.Millisecond})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	pending := dialWSPath(t, server.URL, "/?deviceName=slow-phone", "dGltZW91dA==")
	defer pending.conn.Close()
	waiting := readEvent(t, pending)
	if waiting.Channel != "lan:access-state" || waiting.Payload["state"] != "waiting" {
		t.Fatalf("waiting event = %+v", waiting)
	}
	timeout := readEvent(t, pending)
	if timeout.Channel != "lan:access-state" || timeout.Payload["state"] != "timeout" || timeout.Payload["requestId"] != waiting.Payload["requestId"] {
		t.Fatalf("timeout event = %+v", timeout)
	}
}

func TestRevokeRejectsNextConnectWithToken(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	device, deviceToken, err := manager.ApproveDevice("phone", "10.0.0.9")
	if err != nil {
		t.Fatalf("approve phone: %v", err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "cmV2b2tlLWN0cmw=")
	defer controller.conn.Close()
	_ = readEvent(t, controller)
	sendInvoke(t, controller, 1, "lan:revoke", map[string]any{"deviceId": device.ID})
	revoke := readReply(t, controller)
	if revoke.Error != nil || revoke.Result.(map[string]any)["ok"] != true {
		t.Fatalf("revoke reply = %+v", revoke)
	}

	reconnect := dialWSPath(t, server.URL, "/?deviceToken="+deviceToken, "cmV2b2tlLXBob25l")
	defer reconnect.conn.Close()
	rejected := readEvent(t, reconnect)
	if rejected.Channel != "lan:access-state" || rejected.Payload["state"] != "rejected" || rejected.Payload["reason"] != "invalid-token" {
		t.Fatalf("reconnect after revoke = %+v", rejected)
	}
}

func TestAccessDecideAndRevokeControllerOnly(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	viewer, viewerToken, err := manager.ApproveDevice("viewer", "10.0.0.8")
	if err != nil {
		t.Fatalf("approve viewer: %v", err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	// Registered so the calls below exercise the lease path rather than an
	// unknown channel.
	hub.Register("chat:permissions-pending", func([]any) (any, error) {
		return map[string]any{"permissions": []any{}}, nil
	})
	hub.Register("chat:permission-decide", func([]any) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "Y29udHJvbGxlci1vbmx5LWM=")
	defer controller.conn.Close()
	_ = readEvent(t, controller)
	nonController := dialWSPath(t, server.URL, "/?deviceToken="+viewerToken, "Y29udHJvbGxlci1vbmx5LXY=")
	defer nonController.conn.Close()
	_ = readEvent(t, nonController)

	sendInvoke(t, nonController, 1, "lan:access-decide", map[string]any{"requestId": "missing", "allow": true})
	decide := readReply(t, nonController)
	if decide.Error == nil || !strings.Contains(*decide.Error, `"code":"lan:not-controller"`) {
		t.Fatalf("non-controller access-decide reply = %+v", decide)
	}
	sendInvoke(t, nonController, 2, "lan:revoke", map[string]any{"deviceId": viewer.ID})
	revoke := readReply(t, nonController)
	if revoke.Error == nil || !strings.Contains(*revoke.Error, `"code":"lan:not-controller"`) {
		t.Fatalf("non-controller revoke reply = %+v", revoke)
	}
	// Reads never needed the lease.
	sendInvoke(t, nonController, 3, "chat:permissions-pending")
	pending := readReply(t, nonController)
	if pending.Error != nil {
		t.Fatalf("non-controller pending-permissions must be answered, got %+v", pending)
	}
	if _, ok := pending.Result.(map[string]any)["permissions"]; !ok {
		t.Fatalf("non-controller pending-permissions result = %+v", pending.Result)
	}
	// Ordinary work is not refused: it takes the lease. Asserted on the card the
	// phone exists to answer, because that is the one that used to be a dead end.
	sendInvoke(t, nonController, 4, "chat:permission-decide", map[string]any{"id": "card", "optionId": "allow-once"})
	decidePermission := readReply(t, nonController)
	if decidePermission.Error != nil {
		t.Fatalf("non-controller permission-decide must take the lease, got %+v", decidePermission)
	}
	if !manager.IsController(viewer.ID) {
		t.Fatalf("acting did not move the lease to the acting device")
	}
	holders := 0
	for _, d := range manager.Devices() {
		if manager.IsController(d.ID) {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("lease holders = %d, want exactly 1", holders)
	}
}

// A permission card raised while someone else holds the lease still reaches an
// approved device. Without this the phone the product exists for shows nothing
// until it seizes control, which moves every prompt off the desktop in use.
func TestPermissionEventsReachApprovedNonController(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	_, viewerToken, err := manager.ApproveDevice("phone", "192.168.0.44")
	if err != nil {
		t.Fatalf("approve phone: %v", err)
	}
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "cGVybS12aXNpYmxlLWM=")
	defer controller.conn.Close()
	_ = readEvent(t, controller)
	phone := dialWSPath(t, server.URL, "/?deviceToken="+viewerToken, "cGVybS12aXNpYmxlLXA=")
	defer phone.conn.Close()
	_ = readEvent(t, phone)

	for _, channel := range []string{"chat:permission-request", "chat:permission-resolved"} {
		hub.Broadcast(channel, map[string]any{"id": "card-1", "jobId": "job-1"})
		event := readEvent(t, phone)
		if event.Channel != channel || event.Payload["id"] != "card-1" {
			t.Fatalf("phone %s event = %+v", channel, event)
		}
		if seen := readEvent(t, controller); seen.Channel != channel {
			t.Fatalf("controller %s event = %+v", channel, seen)
		}
	}

	// The screen-addressed events stay exclusive: widening visibility must not
	// have widened these by accident.
	hub.Broadcast("notify", map[string]any{"id": "note-1"})
	if seen := readEvent(t, controller); seen.Channel != "notify" {
		t.Fatalf("controller notify = %+v", seen)
	}
	hub.Broadcast("chat:permission-request", map[string]any{"id": "card-2"})
	if seen := readEvent(t, phone); seen.Channel != "chat:permission-request" || seen.Payload["id"] != "card-2" {
		t.Fatalf("phone saw a notify it should not have; next event = %+v", seen)
	}
}

func TestLocalhostAutoApprove(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: true})
	hub.Register("state:get", func(args []any) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	client := dialWSPath(t, server.URL, "/?deviceName=localhost-browser", "bG9jYWxob3N0LWF1dG8=")
	defer client.conn.Close()
	approved := readEvent(t, client)
	token, _ := approved.Payload["deviceToken"].(string)
	if approved.Channel != "lan:access-state" || approved.Payload["state"] != "approved" || approved.Payload["controller"] != true || len(token) != 64 {
		t.Fatalf("auto approve event channel=%s state=%v controller=%v tokenLen=%d", approved.Channel, approved.Payload["state"], approved.Payload["controller"], len(token))
	}
	sendInvoke(t, client, 1, "state:get")
	state := readReply(t, client)
	if state.Error != nil || state.Result.(map[string]any)["ok"] != true {
		t.Fatalf("auto-approved state reply = %+v", state)
	}
}

func TestControllerLeaseReleasesAfterLastDeviceConnectionDrops(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	replacementToken := approveTestDevice(t, manager, "replacement", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "Y29udHJvbGxlci1kcm9w")
	if approved := readEvent(t, controller); approved.Payload["controller"] != true {
		t.Fatalf("controller access = %+v", approved)
	}
	if err := controller.conn.Close(); err != nil {
		t.Fatalf("close controller: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := manager.Controller(); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller lease remained after its last connection dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}

	replacement := dialWSPath(t, server.URL, "/?deviceToken="+replacementToken, "cmVwbGFjZW1lbnQtY29udHJvbGxlcg==")
	defer replacement.conn.Close()
	approved := readEvent(t, replacement)
	if approved.Payload["controller"] != true {
		t.Fatalf("replacement did not acquire empty controller lease: %+v", approved)
	}
}

func TestControllerLeaseSurvivesWhileSameDeviceHasAnotherConnection(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	first := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "c2FtZS1kZXZpY2UtMQ==")
	if approved := readEvent(t, first); approved.Payload["controller"] != true {
		t.Fatalf("first access = %+v", approved)
	}
	second := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "c2FtZS1kZXZpY2UtMg==")
	defer second.conn.Close()
	if approved := readEvent(t, second); approved.Payload["controller"] != true {
		t.Fatalf("second access = %+v", approved)
	}
	if err := first.conn.Close(); err != nil {
		t.Fatalf("close first connection: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		hub.mu.RLock()
		connected := len(hub.clients)
		hub.mu.RUnlock()
		if connected == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first connection was not dropped; connected=%d", connected)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if controller, ok := manager.Controller(); !ok || controller.Name != "controller" {
		t.Fatalf("controller lease was released while the same device remained connected: controller=%+v ok=%v", controller, ok)
	}
}

func TestOldControllerDisconnectDoesNotClearNewExplicitTakeover(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	replacementToken := approveTestDevice(t, manager, "replacement", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "b2xkLWNvbnRyb2xsZXI=")
	if approved := readEvent(t, controller); approved.Payload["controller"] != true {
		t.Fatalf("controller access = %+v", approved)
	}
	replacement := dialWSPath(t, server.URL, "/?deviceToken="+replacementToken, "bmV3LWNvbnRyb2xsZXI=")
	defer replacement.conn.Close()
	if approved := readEvent(t, replacement); approved.Payload["controller"] != false {
		t.Fatalf("replacement access = %+v", approved)
	}
	sendInvoke(t, replacement, 1, "lan:take-control")
	if reply := readReply(t, replacement); reply.Error != nil || reply.Result.(map[string]any)["controller"] != true {
		t.Fatalf("take-control reply = %+v", reply)
	}
	if err := controller.conn.Close(); err != nil {
		t.Fatalf("close old controller: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		hub.mu.RLock()
		connected := len(hub.clients)
		hub.mu.RUnlock()
		if connected == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old controller connection was not dropped; connected=%d", connected)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if current, ok := manager.Controller(); !ok || current.Name != "replacement" {
		t.Fatalf("old disconnect cleared newer takeover: controller=%+v ok=%v", current, ok)
	}
}

func TestControllerReadyReplayOnlyControllerAndAfterTakeover(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	viewerToken := approveTestDevice(t, manager, "viewer", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	hub.SetOnControllerReady(func(send func(channel string, payload any) error) {
		_ = send("chat:permission-request", map[string]any{"id": "pending-permission"})
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken, "Y3RybC1yZXBsYXk=")
	defer controller.conn.Close()
	if approved := readEvent(t, controller); approved.Channel != "lan:access-state" || approved.Payload["controller"] != true {
		t.Fatalf("controller access = %+v", approved)
	}
	if replay := readEvent(t, controller); replay.Channel != "chat:permission-request" || replay.Payload["id"] != "pending-permission" {
		t.Fatalf("controller replay = %+v", replay)
	}

	viewer := dialWSPath(t, server.URL, "/?deviceToken="+viewerToken, "dmlld2VyLXJlcGxheQ==")
	defer viewer.conn.Close()
	if approved := readEvent(t, viewer); approved.Channel != "lan:access-state" || approved.Payload["controller"] != false {
		t.Fatalf("viewer access = %+v", approved)
	}
	_ = viewer.conn.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	if _, err := viewer.reader.Peek(1); err == nil || !os.IsTimeout(err) {
		t.Fatalf("viewer unexpectedly received controller replay: %v", err)
	}
	_ = viewer.conn.SetReadDeadline(time.Time{})

	sendInvoke(t, viewer, 1, "lan:take-control")
	if reply := readReply(t, viewer); reply.Error != nil || reply.Result.(map[string]any)["controller"] != true {
		t.Fatalf("take-control reply = %+v", reply)
	}
	for i := 0; i < 3; i++ {
		event := readEvent(t, viewer)
		if event.Channel == "chat:permission-request" {
			if event.Payload["id"] != "pending-permission" {
				t.Fatalf("takeover replay = %+v", event)
			}
			return
		}
	}
	t.Fatal("new controller did not receive pending permission replay")
}

func TestNotifyBacklogCapsAtTwentyAndFlushesToController(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	controllerToken := approveTestDevice(t, manager, "controller", "127.0.0.1")
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()

	for i := 0; i < 25; i++ {
		hub.QueueNotify(map[string]any{"title": fmt.Sprintf("notify-%02d", i), "body": "body", "tabId": "cap-tab"})
	}

	controller := dialWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=controller", "bm90aWZ5LWJhY2tsb2c=")
	defer controller.conn.Close()
	approved := readEvent(t, controller)
	if approved.Channel != "lan:access-state" || approved.Payload["controller"] != true {
		t.Fatalf("controller access event = %+v", approved)
	}
	backlog := readEvent(t, controller)
	if backlog.Channel != "notify:backlog" {
		t.Fatalf("backlog event = %+v", backlog)
	}
	items, _ := backlog.Payload["items"].([]any)
	if len(items) != notifyBacklogLimit {
		t.Fatalf("backlog len = %d payload=%#v", len(items), backlog.Payload)
	}
	first := items[0].(map[string]any)
	last := items[len(items)-1].(map[string]any)
	if first["title"] != "notify-05" || last["title"] != "notify-24" {
		t.Fatalf("backlog cap order first=%#v last=%#v", first, last)
	}
}

func TestQueuedControllerEventMovesToNewControllerBeforeWrite(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	oldDevice, _, err := manager.ApproveDevice("old-controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve old controller: %v", err)
	}
	newDevice, _, err := manager.ApproveDevice("new-controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve new controller: %v", err)
	}
	manager.EnsureController(oldDevice)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})

	oldServer, oldPeer := net.Pipe()
	oldClient := addApprovedDirectClient(hub, oldServer, oldDevice)
	defer oldPeer.Close()
	defer hub.drop(oldClient)
	newServer, newPeer := net.Pipe()
	newClient := addApprovedDirectClient(hub, newServer, newDevice)
	defer newPeer.Close()
	defer hub.drop(newClient)
	oldReader := &testWSClient{conn: oldPeer, reader: bufio.NewReader(oldPeer)}
	newReader := &testWSClient{conn: newPeer, reader: bufio.NewReader(newPeer)}

	// Occupy the old controller's writer so the scoped frame is definitely
	// queued when control changes.
	if err := oldClient.sendEvent("ordinary", map[string]any{"seq": 1}); err != nil {
		t.Fatalf("queue blocker: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		frames, bytes := oldClient.outboundSnapshot()
		if frames == 0 && bytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old controller writer did not enter blocked write")
		}
		time.Sleep(time.Millisecond)
	}

	// `show` rather than a permission card: permission VISIBILITY is no longer
	// controller-scoped (2026-07-26), and this test is about re-scoping a frame
	// that was already queued, so it needs a channel that is still exclusive.
	broadcastDone := make(chan struct{})
	go func() {
		hub.Broadcast("show", map[string]any{"id": "show-after-takeover"})
		close(broadcastDone)
	}()
	deadline = time.Now().Add(time.Second)
	for {
		frames, _ := oldClient.outboundSnapshot()
		if frames == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller-only frame was not queued behind blocker")
		}
		time.Sleep(time.Millisecond)
	}
	manager.TakeControl(newDevice)

	if event := readEvent(t, oldReader); event.Channel != "ordinary" {
		t.Fatalf("old controller blocker = %+v", event)
	}
	if event := readEvent(t, newReader); event.Channel != "show" || event.Payload["id"] != "show-after-takeover" {
		t.Fatalf("new controller event = %+v", event)
	}
	select {
	case <-broadcastDone:
	case <-time.After(time.Second):
		t.Fatal("controller event did not finish after takeover delivery")
	}
	_ = oldPeer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := oldReader.reader.Peek(1); err == nil || !os.IsTimeout(err) {
		t.Fatalf("old controller received scoped frame after takeover: %v", err)
	}
}

func TestNotifyBacklogIsRestoredWhenSocketWriteFails(t *testing.T) {
	manager, _, _ := testLeaseManager(t)
	device, _, err := manager.ApproveDevice("controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	manager.EnsureController(device)
	hub := NewHub(Options{Lease: manager, TrustLocalhost: false})
	serverConn, peerConn := net.Pipe()
	client := addApprovedDirectClient(hub, serverConn, device)
	_ = peerConn.Close()

	hub.QueueNotify(map[string]any{"title": "durable", "body": "body"})
	hub.flushNotifyBacklog(client)
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	if len(hub.notifyBacklog) != 1 {
		t.Fatalf("notify backlog length = %d, want 1 after write failure", len(hub.notifyBacklog))
	}
}

func addApprovedDirectClient(hub *Hub, conn net.Conn, device lease.Device) *client {
	client := &client{
		conn:     conn,
		outbound: make(chan outboundFrame, outboundQueueFrameLimit),
		done:     make(chan struct{}),
	}
	client.setDevice(device, "")
	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	hub.mu.Unlock()
	go hub.writeLoop(client)
	return client
}

type decodedEvent struct {
	T       string         `json:"t"`
	Channel string         `json:"channel"`
	Payload map[string]any `json:"payload"`
}

func testLeaseManager(t *testing.T) (*lease.Manager, *bytes.Buffer, string) {
	t.Helper()
	var logs bytes.Buffer
	stateDir := t.TempDir()
	manager, err := lease.NewManager(lease.Options{
		StateDir: stateDir,
		Logf: func(format string, args ...any) {
			logs.WriteString(strings.TrimSpace(formatMessage(format, args...)))
			logs.WriteByte('\n')
		},
	})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}
	return manager, &logs, stateDir
}

func approveTestDevice(t *testing.T, manager *lease.Manager, name, ip string) string {
	t.Helper()
	_, token, err := manager.ApproveDevice(name, ip)
	if err != nil {
		t.Fatalf("approve test device: %v", err)
	}
	return token
}

func sendInvoke(t *testing.T, client *testWSClient, id int, channel string, args ...any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		t.Fatalf("marshal invoke: %v", err)
	}
	client.sendText(t, string(payload))
}

func readEvent(t *testing.T, client *testWSClient) decodedEvent {
	t.Helper()
	var event decodedEvent
	if err := json.Unmarshal(client.readText(t), &event); err != nil {
		t.Fatalf("event json: %v", err)
	}
	return event
}

func formatMessage(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}
