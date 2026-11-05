import type { Chat } from '../store/types';
import { store } from '../store/store';
import { connectedArtifactURL } from '../connected-artifacts';
import { OUTPUT_TAG, type ChatOutput } from '../chat-outputs';
import { loadImageSrc } from './MessageImage';

// One output: its kind, its title, and «Abrir», which does what opening it in
// the chat does — a hosted page or file goes to the chat's browser (or a new
// window where there is none), an image to the viewer.
function OutputRow({ chat, output }: { chat: Chat; output: ChatOutput }) {
  const origin = store.browserArtifactMachineId(chat.machineId);
  let open;
  if (output.target) {
    const target = output.target;
    const url = connectedArtifactURL(origin, target, undefined, undefined, store.localMachineId());
    open = (
      <a className="rp-open" href={url || undefined} target="_blank" rel="noreferrer" onClick={(event) => {
        if (store.openHostedArtifact(chat.id, target, origin)) event.preventDefault();
        else if (!url) event.preventDefault();
      }}>Abrir</a>
    );
  } else if (output.image) {
    const image = output.image;
    open = (
      <button type="button" className="rp-open" onClick={() => {
        void loadImageSrc(chat.id, image).then((src) => { if (src) store.openImageLightbox(src, output.title); });
      }}>Abrir</button>
    );
  }
  return (
    <div className="rp-out" data-kind={output.kind}>
      <span className="rp-out-k">{OUTPUT_TAG[output.kind]}</span>
      <span className="rp-out-t" title={output.title}>{output.title}</span>
      {open}
    </div>
  );
}

// «Salidas» — what this chat produced that a person opens (chat-outputs.ts),
// newest first, flat rows against the pane.
export function SalidasTab({ chat, outputs }: { chat: Chat; outputs: readonly ChatOutput[] }) {
  if (outputs.length === 0) return <div className="rp-empty">Nada todavía</div>;
  return (
    <div className="rp-outs">
      {outputs.map((output) => <OutputRow key={output.key} chat={chat} output={output} />)}
    </div>
  );
}
