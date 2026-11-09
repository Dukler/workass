'use strict';

// Ajustes · Mantenimiento: the two folders the shell can show, Workass's data
// («Mostrar en Finder») and its logs («Abrir carpeta»). Both paths are this
// shell's own profile paths (runtime-profile.js), which are also the ones it
// hands the daemon it starts (--state-dir, WORKASS_LOG_ROOT). The page only
// names which of the two it wants; it never sends a path, so it can make the
// shell show these folders and nothing else, and a folder is only shown,
// never launched.

const FOLDERS = {
  data: { key: 'stateDir', missing: 'La carpeta de datos todavía no existe.' },
  logs: { key: 'logRoot', missing: 'La carpeta de registros todavía no existe.' },
};

function isDirectory(fs, dir) {
  try { return fs.statSync(dir).isDirectory(); } catch { return false; }
}

async function showMaintenanceFolder(kind, { runtime, shell, fs }) {
  const folder = Object.prototype.hasOwnProperty.call(FOLDERS, kind) ? FOLDERS[kind] : null;
  if (!folder) return { ok: false, error: 'unknown folder' };
  const dir = runtime && typeof runtime[folder.key] === 'string' ? runtime[folder.key] : '';
  if (!dir || !isDirectory(fs, dir)) return { ok: false, error: folder.missing };
  if (kind === 'data') {
    // Revealed in its parent, so the data folder is shown selected.
    shell.showItemInFolder(dir);
    return { ok: true };
  }
  const error = await shell.openPath(dir);
  return error ? { ok: false, error: String(error) } : { ok: true };
}

module.exports = { showMaintenanceFolder };
