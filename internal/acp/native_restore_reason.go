package acp

type nativeRestoreFallbackReason string

const (
	nativeRestoreFallbackNoBinding         nativeRestoreFallbackReason = "no-binding"
	nativeRestoreFallbackResumeUnsafe      nativeRestoreFallbackReason = "resume-unsafe"
	nativeRestoreFallbackCwdMismatch       nativeRestoreFallbackReason = "cwd-mismatch"
	nativeRestoreFallbackHistoryDivergence nativeRestoreFallbackReason = "history-divergence"
	nativeRestoreFallbackProviderMiss      nativeRestoreFallbackReason = "provider-miss"
	nativeRestoreFallbackUnsupported       nativeRestoreFallbackReason = "unsupported"
)

type nativeRestoreFallbackState struct {
	BindingFound     bool
	ResumeSafe       bool
	CWDMatches       bool
	HistoryMatches   bool
	RestoreSupported bool
	RestoreAttempted bool
	RestoreFailed    bool
}

func nativeRestoreFallbackReadyState() nativeRestoreFallbackState {
	return nativeRestoreFallbackState{
		BindingFound:     true,
		ResumeSafe:       true,
		CWDMatches:       true,
		HistoryMatches:   true,
		RestoreSupported: true,
	}
}

func nativeRestoreFallbackReasonFor(state nativeRestoreFallbackState) nativeRestoreFallbackReason {
	switch {
	case !state.BindingFound:
		return nativeRestoreFallbackNoBinding
	case !state.ResumeSafe:
		return nativeRestoreFallbackResumeUnsafe
	case !state.CWDMatches:
		return nativeRestoreFallbackCwdMismatch
	case !state.HistoryMatches:
		return nativeRestoreFallbackHistoryDivergence
	case !state.RestoreSupported:
		return nativeRestoreFallbackUnsupported
	case state.RestoreAttempted && state.RestoreFailed:
		return nativeRestoreFallbackProviderMiss
	default:
		return ""
	}
}
