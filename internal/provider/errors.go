package provider

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorKind string

const (
	ErrorTransientTransport     ErrorKind = "transient_transport"
	ErrorAuthenticationRequired ErrorKind = "authentication_required"
	ErrorProviderUnavailable    ErrorKind = "provider_unavailable"
	ErrorNativeThreadMissing    ErrorKind = "native_thread_missing"
	ErrorNativeIdentityConflict ErrorKind = "native_identity_conflict"
	ErrorUnsupportedCapability  ErrorKind = "unsupported_capability"
	ErrorAdmissionRejected      ErrorKind = "admission_rejected"
	ErrorAcceptanceAmbiguous    ErrorKind = "acceptance_ambiguous"
	ErrorPermissionPending      ErrorKind = "permission_pending"
	ErrorProtocolViolation      ErrorKind = "protocol_violation"
	ErrorContextLimitReached    ErrorKind = "context_limit_reached"
)

type Error struct {
	Kind      ErrorKind
	Operation OperationID
	Message   string
	Cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = string(e.Kind)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", message, e.Cause)
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func ErrorIs(err error, kind ErrorKind) bool {
	var providerErr *Error
	return errors.As(err, &providerErr) && providerErr.Kind == kind
}

func Unsupported(operation OperationID, message string) error {
	return &Error{Kind: ErrorUnsupportedCapability, Operation: operation, Message: message}
}
