package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"workass/internal/lease"
)

// A chat image is fetched by URL, not over the socket, so the browser fetches
// and decodes it off the renderer's main thread and caches it by URL. An <img>
// cannot send a header, so the request carries its credential in the URL. That
// credential is never the device token: a URL lands in the DOM, in "copy image
// address" and in caches, and the token authorizes everything a device can do.
//
// The image key is a read-only capability derived from the device record:
// "<deviceId>.<HMAC-SHA256(tokenHash, label|deviceId)[:16] hex>". The daemon
// hands it to an approved device on lan:access-state (additive field
// `imageKey`) and recomputes it per request, so nothing new is stored. It
// stays the same across restarts, which keeps image URLs, and the browser's
// cache of them, stable. Revoking or re-pairing a device changes or removes
// the record it is derived from, and every URL carrying the old key stops
// working at once. The key opens no chat by itself: each request still passes
// the archive image read's ownership check for the chat it names.
const chatImageKeyLabel = "workass chat-image key v1\x00"

func chatImageKey(device lease.Device) string {
	id := strings.TrimSpace(device.ID)
	if id == "" || strings.TrimSpace(device.TokenHash) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(device.TokenHash))
	_, _ = mac.Write([]byte(chatImageKeyLabel + id))
	return id + "." + hex.EncodeToString(mac.Sum(nil)[:16])
}

// AuthorizeChatImageKey reports whether key is the image key of a device that
// is still approved. A hub without device pairing approves every socket, and
// so every image request, exactly as it does on the wire.
func (h *Hub) AuthorizeChatImageKey(key string) bool {
	if h == nil {
		return false
	}
	if h.lease == nil {
		return true
	}
	key = strings.TrimSpace(key)
	deviceID, _, ok := strings.Cut(key, ".")
	if !ok || deviceID == "" {
		return false
	}
	device, ok := h.lease.Device(deviceID)
	if !ok {
		return false
	}
	want := chatImageKey(device)
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(key)) == 1
}
