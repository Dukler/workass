package chat

import "workass/internal/provider"

// AdmissionFailureMessage is shared by the durable transcript and the rejected
// invocation reply so event/reply ordering cannot change the user's explanation.
func AdmissionFailureMessage(kind provider.ErrorKind) string {
	if kind == provider.ErrorModelUnavailable {
		return "The selected model is no longer available from this provider. Choose an available model in the model selector, then send your message again. Your message has been kept in this chat and was not sent to the provider."
	}
	message := "The provider could not start this turn."
	if kind != "" {
		message += " " + string(kind) + "."
	}
	return message
}
