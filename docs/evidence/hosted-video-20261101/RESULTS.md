# Hosted video does not play: evidence, 2026-11-01

Every run is the real daemon (scratch state), the real shell and renderer in the pinned
Electron, and the real assistant Markdown link clicked in a real chat. All playback is muted
from WebContents creation (`AGENTS.md`), recorded as `muted_at_end` and `video_*.muted`.
The rig window never takes focus, so occlusion handling is switched off for it.

Each folder's `results.json` holds the timeline, request log and alerts.

- `before/`: unfixed commit f631976d. Empty 300x150 player, `readyState` 0, `origin` null,
  CORS error on the media request, and a false "No se pudo abrir el artefacto" toast.
- `control-plain-electron.json`: the file with no Workass code. It plays with and without
  Range support; only the `sandbox` CSP stops it, with the same CORS error.
- `after/`: the same file fixed. Real picture in `page-playing.png`, controls in
  `page-paused.png`, the pane in `window.png`. Moov-at-end Range requests, no alerts.
- `remote/`: owned by a second daemon, reached through the paired socket. Direct LAN HTTP
  stays 403.
- `matrix/`: 640x360 moov-at-end mp4, 320x180 webm, portrait mp4, mp3, and a 120 MB
  moov-at-end mp4 through far seeks and a 40 s pause.
- `published/`: the published 0.1.259 Mac zip's daemon and renderer. Its shell files are
  byte-identical to the repo's and run from there, because the bundle's shell needs its sealed
  layout. Nothing was installed.
- `control-disguised-markup.json`: HTML served as a hosted .mp4 and .webm runs no script.

Rerun: `node harness/vrig.mjs BIN LABEL [--file F] [--out DIR] [--remote] [--stress] [--shell DIR]`.
Build the `cgwin` helper first:
`swiftc -O -o "${TMPDIR}vrig-cgwin" ../agent-browser-hidden-20261031/harness/cgwin.swift`
