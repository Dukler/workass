package acp

import "testing"

func TestNativeRestoreFallbackReasonFor(t *testing.T) {
	base := nativeRestoreFallbackReadyState()
	tests := []struct {
		name string
		edit func(*nativeRestoreFallbackState)
		want nativeRestoreFallbackReason
	}{
		{
			name: "no-binding",
			edit: func(state *nativeRestoreFallbackState) {
				state.BindingFound = false
			},
			want: nativeRestoreFallbackNoBinding,
		},
		{
			name: "resume-unsafe",
			edit: func(state *nativeRestoreFallbackState) {
				state.ResumeSafe = false
			},
			want: nativeRestoreFallbackResumeUnsafe,
		},
		{
			name: "cwd-mismatch",
			edit: func(state *nativeRestoreFallbackState) {
				state.CWDMatches = false
			},
			want: nativeRestoreFallbackCwdMismatch,
		},
		{
			name: "history-divergence",
			edit: func(state *nativeRestoreFallbackState) {
				state.HistoryMatches = false
			},
			want: nativeRestoreFallbackHistoryDivergence,
		},
		{
			name: "provider-miss",
			edit: func(state *nativeRestoreFallbackState) {
				state.RestoreAttempted = true
				state.RestoreFailed = true
			},
			want: nativeRestoreFallbackProviderMiss,
		},
		{
			name: "unsupported",
			edit: func(state *nativeRestoreFallbackState) {
				state.RestoreSupported = false
			},
			want: nativeRestoreFallbackUnsupported,
		},
		{
			name: "none",
			edit: func(state *nativeRestoreFallbackState) {},
			want: "",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			state := base
			tc.edit(&state)
			if got := nativeRestoreFallbackReasonFor(state); got != tc.want {
				t.Fatalf("nativeRestoreFallbackReasonFor(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
