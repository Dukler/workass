// macOS supplies the traffic lights through Electron's hidden-inset titlebar.
// Windows uses the same left-side visual language, but needs explicit controls
// because the shell is frameless there.
export function WindowControls() {
  const bridge = typeof window !== 'undefined' ? window.workassWindow : undefined;
  if (bridge?.platform !== 'win32') return null;

  const control = (action: 'minimize' | 'toggle-maximize' | 'close') => {
    void bridge.control(action);
  };

  return (
    <div className="window-controls" aria-label="Controles de ventana">
      <button className="window-control window-control-close" aria-label="Cerrar" title="Cerrar" onClick={() => control('close')} />
      <button className="window-control window-control-minimize" aria-label="Minimizar" title="Minimizar" onClick={() => control('minimize')} />
      <button className="window-control window-control-maximize" aria-label="Maximizar" title="Maximizar" onClick={() => control('toggle-maximize')} />
    </div>
  );
}
