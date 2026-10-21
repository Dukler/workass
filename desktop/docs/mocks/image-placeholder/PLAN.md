# Image placeholder: reserve the real size

Status: mock for approval (`index.html`, `shots/`). Nothing is built. The user
picks a variant first; this plan is closed apart from that choice.

## The problem

A chat image sent by reference (archive reads with `deferImages`, tool media)
paints as one line of text until it scrolls into view. Then it grows to its real
height and pushes every row below it.

| Surface | Today, before the bytes | Code |
| --- | --- | --- |
| User attachment | `span.assistant-image-pending`, one text line (~17 px) | `messages.tsx` `UserImage` |
| Assistant inline image / gallery | the same span inside the bordered button | `MessageImage.tsx` `MessageImageButton` |
| Tool image | `span.tool-image-pending`, `min-height: 170px`; the image is up to 340 px | `messages.tsx` `ToolImage` |

In the mock's sample chat (two attachments, one chart, three tool images), the
row under them moves **1014 px** while the images load.

The renderer cannot reserve space because nothing on the wire says how big an
image is. A wire image is `{mimeType, name, source?, deferredImageRef, data: ""}`
(`projectionAttachments`, `deferArchiveMessageImages`, `deferArchiveToolImages`
in `cmd/workass/provider_chat_projection.go`). The durable
`provider.Attachment` (`internal/provider/contract.go`) has `ID, Name, MIMEType,
Digest, Size, Source, Ref` and no dimensions.

## The fix

The daemon sends `width` and `height` (image pixels) on every image it can read.
The renderer gives the image's frame its final size on the first paint, using
today's caps, so swapping the placeholder for the image changes no layout. In
the mock the same row moves **0 px**. Against an old daemon with no sizes, the
frame uses one default size per surface and corrects once on load (**188 px** in
the sample).

## Variants (pick one)

The variants differ only in how the reserved frame looks while the bytes are on
the way. All of them use neutral surfaces, keep today's copy ("Cargando
imagen…", "Imagen no disponible") and run their motion unconditionally. This
Mac has Reduce Motion on, and nothing is gated on `prefers-reduced-motion`.

- **A · Reserva quieta.** The exact frame on `--field`, with an image glyph and
  "Cargando imagen…" centred. A short frame shows only the glyph. No motion
  except a 160 ms fade-in of the image.
- **B · Barrido.** The same frame, glyph only, with a slow neutral sweep (7 %
  ink, 1.6 s). It shows that bytes are on the way without text or colour. On a
  620 px chart, though, it is a large moving surface.
- **C · Línea de acento.** A's frame plus a 2 px accent bar running along the
  bottom edge. The only green is the progress mark, as with the "Trabajando"
  orbit.

**Recommendation: C.** It shows that bytes are arriving without moving a large
surface, keeps green as an accent, and matches how Workass already marks
progress. A is the fallback if any motion on an image frame feels like too much.

## Daemon (additive)

1. **One header reader.** Move `imageDimensions` and `webpDimensions` out of
   `internal/acp/fork_images.go` into a small `internal/imagesize` package. It
   uses only the standard library: `image.DecodeConfig` with png, jpeg and gif
   registered, plus the existing hand-written WebP chunk parser. Add a reader
   form: `DecodeConfig` on a stream reads only the header. `fork_images.go` calls
   the moved function, with no behaviour change.
2. **Durable fields.** `provider.Attachment` gains `Width, Height int` with
   `json:",omitempty"`. An attachment without a size encodes byte-identically, so
   existing rows, SQLite `messages` diffs and the file journal do not churn. An
   older build ignores the unknown keys, since actor decoding does not use
   `DisallowUnknownFields`.
3. **Fill at ingestion.** One constructor builds every durable attachment and
   already holds the bytes: `(*sessionStore).PlanProviderAttachments`
   (`cmd/workass/session_store.go`). It covers user input, queue items and
   `PersistProviderAttachments`, which handles every assistant and tool image
   from a provider. The payload is base64 text, so it reads the header with
   `base64.NewDecoder` + `imagesize`. If the header can't be read (SVG, damaged
   bytes), the size stays absent and the image is unaffected.
   `steerAttachmentInputIdentity` (`provider_chat_runtime.go`) recomputes "the
   same immutable attachment identity" for steering. It must produce the same
   size, or its comparison must ignore the size.
4. **Older rows.** Add one projection post-pass, `annotateSessionImageSizes(messages,
   stateDir)`. For each image map that still has `_workassImageDataRef` and no
   size, it reads the content-addressed sidecar's header. Results go into a
   bounded process cache keyed by digest; the content is immutable, and failures
   are cached too. The pass runs where `rehydrateExternalSessionImages` and
   `deferArchive*Images` run (session:get, `archive-load` page/recent/full),
   before deferral strips the ref field. It never rewrites actor state, so no
   migration is needed.
5. **Wire.** `width` and `height` are integers on each image object in
   `session:get` and every `archive-load` form. Live `job:event` images carry
   inline bytes and never show a placeholder, so they are unchanged.

## Renderer

1. `MessageImage` (`src/store/types.ts`) gains `width?: number; height?: number`.
2. One helper in `MessageImage.tsx`: `imageFrameStyle(image, surface)`. It returns
   the CSS variables `--iw` and `--ih`. Without a size it falls back per surface:
   user 640×480, inline 1600×900, gallery and tool 1200×900.
3. **The frame owns the size.** `UserImage`, `MessageImageButton` (inline and
   inside `.assistant-images`) and `ToolImage` render a frame with an inner
   media box that carries `aspect-ratio: var(--iw) / var(--ih)`. The placeholder
   and the `<img>` both fill that box (`object-fit: contain`), so the swap is
   layout-neutral. Per the identifier law, the existing classes
   (`.assistant-inline-image`, `.tool-image`, `.user-images img`,
   `.assistant-image-pending`, `.tool-image-pending`) stay as aliases. The new
   `ri`, `ri-media` and `ri-ph` classes are added beside them.
4. **CSS.** Port the mock's `.ri*` rules and the chosen variant. The caps are
   unchanged: attachment `min(420px, 68vw)` × 320, inline 620 high, tool 340.
   Attachments keep stretching to their row's tallest item, as today's `<img>`
   does, with the media centred. The 170 px `min-height` on
   `.tool-image-pending` is dropped in favour of the reservation.
5. **Unchanged:** lazy loading (`observeTranscriptImageOnce`, 400 px margin),
   the image cache, click-to-enlarge, the lightbox, copy-image, and
   `transcript-window.ts`. A unit's first measured height is now final, so the
   anchor has nothing to correct.

## Compatibility

| Renderer | Daemon | Result |
| --- | --- | --- |
| new | new | exact frame, nothing moves |
| new | old | default frame, one correction per image on load |
| old | new | ignores `width`/`height`; today's behaviour |
| older build | state written by new | `Width`/`Height` keys ignored |

## Tests

- Go, `internal/imagesize`: png, jpeg and gif fixtures from the stdlib encoders,
  and handmade VP8, VP8L and VP8X headers. Undecodable and SVG input returns no
  size. The reader form reads only the header (a counting reader).
- Go, `cmd/workass`:
  - an attachment without a size marshals with no `Width` key (byte-identical
    golden);
  - `PlanProviderAttachments` fills the size, and the steering identity still
    matches the planned attachment;
  - an older attachment gains a size from its sidecar through the post-pass;
  - deferred message and tool images keep `width`/`height` after deferral;
  - the lean/budget page path is unchanged;
  - the SQLite golden and snapshot parity tests still pass.
- Renderer:
  - `imageFrameStyle` math and fallbacks;
  - for each surface, with and without a size, `renderToStaticMarkup` gives the
    placeholder and the loaded markup the same frame style;
  - update the markup assertions in `assistant-images.test.ts`,
    `deferred-message-images.test.ts` and `tool-group-lazy.test.ts`.

## Gates

- `go build ./...`; `go test -count=1 ./internal/imagesize/... ./internal/acp/...
  ./cmd/workass/...`; `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/workass`.
- `npm run typecheck`, `npm test`, `npm run build` in `desktop/renderer2`.
- Check it on the running dev window; served is not delivered. Open a chat with
  deferred images, then DOM-probe (`/__workass-shell/probe`) the top of the row
  under an image before and after it loads. The delta must be 0. Take a
  screenshot in both themes.

## Check before building

- Whether any operation digest or receipt hashes an `Attachment` by its JSON.
  Start with the steering identity above and the queue mutation receipts. A
  digest recorded by an older build and replayed by a newer one would then
  differ. If one does, fill the size after the digest boundary or leave it out
  of the digest.

## Out of scope

- Blur-up or dominant-colour placeholders (another wire field, and a large
  colour surface).
- Fork-child thumbnails (`.kthumb`, already a fixed 92×83).

## Mock

`index.html` is self-contained and uses the real tokens from `tokens.css`. Its
transcript CSS is verbatim from `app.css`. URL parameters:

- `theme=dark|light`
- `v=a|b|c|hoy`
- `state=pending|mixed|loaded|failed`
- `dims=0` (old daemon)
- `replay=1`

The readout measures the reference row on offscreen copies of the transcript,
with nothing loaded and with everything loaded. `shots/` has each variant and
Hoy in both themes, C without sizes in both themes, and C with a failed image.
All shots are 1280 px wide, from headless Brave over the DevTools protocol.
