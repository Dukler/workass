# Workass idea bank

This is the parking lot for promising product and architecture ideas that are
not yet binding work. Entries preserve the motivation, evidence, constraints,
and unanswered decisions so they can be reconsidered without quietly changing
`docs/PORT-SPEC.md` or the master plan.

## Entry template

- **Status:** exploring | parked | promoted | rejected
- **Problem:** What user-visible problem are we solving?
- **Candidate:** What is the proposed direction?
- **Why it may work:** Evidence and useful properties.
- **Constraints / risks:** What could make it unsuitable?
- **Questions before promotion:** Decisions or experiments still required.
- **Sources:** Primary references used during research.

## Server-owned browser with shared SSO and agent control

- **Status:** parked — research before committing to an implementation
- **Problem:** An Electron `WebContentsView` belongs to the client machine. A
  LAN client would therefore get a different browser, profile, cookies, and
  visual state. Workass needs one browser owned by the always-on server so all
  clients see the same session and agents can control that same browser.
- **Candidate:** Run a dedicated browser process beside the Workass daemon.
  Keep its persistent profile, cookies, downloads, and SSO state on the server;
  use CDP/Playwright for agent automation; send the human-visible browser over
  a real interactive video transport such as WebRTC. Map Workass's controller
  lease to input authority so observers can watch but only the controller can
  click or type.
- **Why it may work:**
  - Neko already implements a server-side browser/desktop streamed over
    WebRTC, supports persistent state, and can be combined with browser
    automation while the user watches.
  - BrowserBox is a closer turnkey cross-platform product with an embeddable
    browser surface and automation API.
  - CDP remains useful as the control protocol, but not as the pixel transport;
    CDP screencasting has materially worse frame rate and stability than
    OS-level WebRTC streaming.
- **Constraints / risks:**
  - Neko is Linux/Docker-first, conflicting with Workass's single native
    Windows/macOS artifact goal.
  - BrowserBox is commercial, closed-source, and requires a product key.
  - WebRTC normally introduces ICE/NAT and UDP/TURN requirements; Workass's
    Windows environment may expose only TCP port 80.
  - A persistent Workass browser profile provides shared login state, but does
    not automatically inherit an existing managed Chrome/Edge profile. Device
    certificates and enterprise browser policy need real-site testing.
  - KasmVNC, Selkies, and Guacamole solve broader remote-desktop problems but
    are heavier than the browser-specific requirement.
- **Questions before promotion:**
  1. Is a Linux/Docker browser sidecar acceptable on every Workass server?
  2. Must the final runtime remain a single cross-platform artifact?
  3. Can the target network allow WebRTC UDP, or must media tunnel through the
     existing HTTP port?
  4. Which actual SSO sites require managed-device or client-certificate trust?
  5. Should Workass trial Neko first, or evaluate BrowserBox licensing and
     native cross-platform behavior first?
  6. How are clipboard, downloads, uploads, permission prompts, and concurrent
     agent/user input represented in the controller lease?
- **Suggested spike:** Put Neko behind an isolated development-only adapter,
  embed its client in the browser rail, persist one test profile, attach an
  automation client to the same browser, and verify reconnect plus LAN viewing.
  Do not extend the current Electron-local browser or build a custom CDP image
  streamer during this spike.
- **Sources:**
  - Neko: <https://github.com/m1k1o/neko>
  - BrowserBox: <https://github.com/BrowserBox/BrowserBox>
  - BrowserBox evaluation terms: <https://browserbox.io/evaluate>
  - Steel Browser: <https://github.com/steel-dev/steel-browser>
  - Steel's WebRTC rendering notes: <https://steel.dev/blog/webrtc>
  - Browserless self-hosting: <https://docs.browserless.io/enterprise/open-source>
  - Selkies: <https://github.com/selkies-project/selkies>
  - Electron offscreen rendering: <https://www.electronjs.org/docs/latest/tutorial/offscreen-rendering>
