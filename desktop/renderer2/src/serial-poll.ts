// Schedule the next read only after its predecessor settles. A slow remote
// connection must not accumulate requests, and cleanup must remain final even
// when the outstanding read completes after its component has gone away.
export function startSerialPoll(read: () => Promise<void>, intervalMs: number): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const refresh = async () => {
    try { await read(); }
    catch { /* The read owns its error presentation; a rejection is not a timer leak. */ }
    finally {
      if (!stopped) timer = setTimeout(() => { void refresh(); }, intervalMs);
    }
  };
  void refresh();
  return () => {
    stopped = true;
    if (timer !== undefined) clearTimeout(timer);
  };
}
