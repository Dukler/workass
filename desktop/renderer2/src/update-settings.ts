// "Instalar solo" (Ajustes · Actualizaciones). The owner's choice lives in the
// daemon's app-settings blob under "autoInstallUpdates" (cmd/workass
// config_store.go normalizeSettings, off by default); the Mac shell's
// UpdateManager acts on it, so the renderer hands every value it adopts to the
// shell through window.workassUpdater.setAutoInstall.

export const AUTO_INSTALL_UPDATES_KEY = 'autoInstallUpdates';

export function autoInstallUpdatesFromSettings(raw: unknown): boolean {
  if (!raw || typeof raw !== 'object') return false;
  const settings = (raw as Record<string, unknown>).settings;
  if (!settings || typeof settings !== 'object') return false;
  return (settings as Record<string, unknown>)[AUTO_INSTALL_UPDATES_KEY] === true;
}

export function withAutoInstallUpdatesInSettings(current: Record<string, unknown> | null | undefined, enabled: boolean): Record<string, unknown> {
  return { ...(current ?? {}), [AUTO_INSTALL_UPDATES_KEY]: enabled === true };
}

/** Hands the setting to this machine's shell. A browser client has no shell and
 *  an older shell has no setter: both simply do not install by themselves. */
export function pushAutoInstallToShell(enabled: boolean): void {
  const updater = typeof window === 'undefined' ? undefined : window.workassUpdater;
  if (typeof updater?.setAutoInstall !== 'function') return;
  void updater.setAutoInstall(enabled === true).catch(() => undefined);
}
