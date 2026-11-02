# An agent's hidden page must not reach the speakers

`harness/rig.mjs`: scratch daemon with `--state-dir` and the mock provider, real shell and renderer in
the pinned Electron, the real `workass tools call`. An agent hosts a 30 s video with a 440 Hz tone, opens
it with `workass_browser_open` (hidden), then asks to show it with `visible:true`. `before.json` is
main's shell, `after.json` this branch's.

The rig never sounds: Chromium's output is off (`mute-audio`), every page is muted from creation, and a
page's `setAudioMuted` is recorded rather than applied. So the proof is what the shell asked for, not
what came out of a speaker. Nobody listened.

| Step | Before | After |
| --- | --- | --- |
| Hidden, video playing, page's own `muted` false | never asked to mute | muted |
| Shown in the panel | never asked | sound on |

Run with `node harness/rig.mjs BIN SHELL_DIR RENDERER_DIR ELECTRON_APP REPO LABEL`.
