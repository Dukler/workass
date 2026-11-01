package scheduledmessage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxMessageBytes      = 32 * 1024
	maxOperationIDLen    = 200
	maxDelaySeconds      = 604800
	minExpirySeconds     = 60
	defaultExpirySeconds = 86400
	maxAhead             = 7 * 24 * time.Hour
)

// Request is a validated scheduled-message request, ready for Schedule.
type Request struct {
	OperationID string
	Message     string
	// Field is the raw tool parameter that carried the condition ("at",
	// "after_seconds", "when_tcp_up", "when_file_exists", "when_pid_exits"),
	// Value its normalized raw value. Both feed RequestHash.
	Field        string
	Value        string
	Condition    Condition     // At is zero for after_seconds until scheduled
	After        time.Duration // set for after_seconds
	ExpiresAfter time.Duration // zero for "at" and after_seconds
}

var conditionParams = []string{"at", "after_seconds", "when_tcp_up", "when_file_exists", "when_pid_exits"}

// ParseRequest validates the tool params of a schedule call. Errors are plain
// sentences meant for the calling agent.
func ParseRequest(params map[string]any) (Request, error) {
	var req Request
	message, err := stringParam(params, "message")
	if err != nil {
		return req, err
	}
	if message == "" {
		return req, errors.New("message is required and must not be empty")
	}
	if len(message) > maxMessageBytes {
		return req, fmt.Errorf("message is too long: at most %d bytes are allowed", maxMessageBytes)
	}
	operationID, err := stringParam(params, "operation_id")
	if err != nil {
		return req, err
	}
	if operationID == "" {
		return req, errors.New("operation_id is required: pass a stable id of your choice so a retried call does not schedule a second message")
	}
	if len(operationID) > maxOperationIDLen {
		return req, fmt.Errorf("operation_id is too long: at most %d characters are allowed", maxOperationIDLen)
	}
	var present []string
	for _, key := range conditionParams {
		if v, ok := params[key]; ok && v != nil {
			present = append(present, key)
		}
	}
	if len(present) != 1 {
		return req, errors.New("pass exactly one of at, after_seconds, when_tcp_up, when_file_exists, when_pid_exits")
	}
	req.OperationID = operationID
	req.Message = message
	req.Field = present[0]
	expires, hasExpires, err := intParam(params, "expires_after_seconds")
	if err != nil {
		return req, err
	}
	switch req.Field {
	case "at":
		raw, err := stringParam(params, "at")
		if err != nil {
			return req, err
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return req, errors.New("at must be an RFC3339 timestamp such as 2026-10-16T18:30:00Z")
		}
		at = at.UTC()
		req.Value = at.Format(time.RFC3339Nano)
		req.Condition = Condition{Kind: "at", At: at}
	case "after_seconds":
		n, _, err := intParam(params, "after_seconds")
		if err != nil {
			return req, err
		}
		if n < 1 || n > maxDelaySeconds {
			return req, fmt.Errorf("after_seconds must be between 1 and %d", maxDelaySeconds)
		}
		req.Value = strconv.FormatInt(n, 10)
		req.After = time.Duration(n) * time.Second
		req.Condition = Condition{Kind: "at"}
	case "when_tcp_up":
		raw, err := stringParam(params, "when_tcp_up")
		if err != nil {
			return req, err
		}
		host, port, err := net.SplitHostPort(raw)
		if err != nil || host == "" || port == "" {
			return req, errors.New("when_tcp_up must be host:port, for example 127.0.0.1:8080")
		}
		req.Value = raw
		req.Condition = Condition{Kind: "tcp", Addr: raw}
	case "when_file_exists":
		raw, err := stringParam(params, "when_file_exists")
		if err != nil {
			return req, err
		}
		if !filepath.IsAbs(raw) {
			return req, errors.New("when_file_exists must be an absolute path")
		}
		req.Value = raw
		req.Condition = Condition{Kind: "file", Path: raw}
	case "when_pid_exits":
		n, _, err := intParam(params, "when_pid_exits")
		if err != nil {
			return req, err
		}
		if n <= 1 || n > math.MaxInt32 {
			return req, errors.New("when_pid_exits must be a process id greater than 1")
		}
		req.Value = strconv.FormatInt(n, 10)
		req.Condition = Condition{Kind: "pid", PID: int(n)}
	}
	if req.Condition.Kind == "at" {
		if hasExpires {
			return req, errors.New("expires_after_seconds only applies to when_tcp_up, when_file_exists and when_pid_exits")
		}
		return req, nil
	}
	if !hasExpires {
		expires = defaultExpirySeconds
	}
	if expires < minExpirySeconds || expires > maxDelaySeconds {
		return req, fmt.Errorf("expires_after_seconds must be between %d and %d", minExpirySeconds, maxDelaySeconds)
	}
	req.ExpiresAfter = time.Duration(expires) * time.Second
	return req, nil
}

// hash covers the raw request fields, never the computed At, so a retried
// after_seconds call hashes identically.
func (r Request) hash() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.Message, r.Field, r.Value, strconv.FormatInt(int64(r.ExpiresAfter/time.Second), 10),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func stringParam(params map[string]any, key string) (string, error) {
	v, ok := params[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(s), nil
}

// intParam reads a whole number; JSON numbers arrive as float64.
func intParam(params map[string]any, key string) (int64, bool, error) {
	v, ok := params[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	bad := fmt.Errorf("%s must be a whole number", key)
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || math.IsInf(n, 0) || math.Abs(n) > 1<<53 {
			return 0, true, bad
		}
		return int64(n), true, nil
	case int:
		return int64(n), true, nil
	case int64:
		return n, true, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, true, bad
		}
		return i, true, nil
	}
	return 0, true, bad
}
