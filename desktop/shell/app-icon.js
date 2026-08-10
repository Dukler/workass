'use strict';

const fs = require('node:fs');
const path = require('node:path');

function resolveAppIconPath({ isPackaged, resourcesPath, repoRoot }) {
  const candidates = isPackaged
    ? [
        path.join(resourcesPath, 'Workass.png'),
        path.join(resourcesPath, 'Workass.icns'),
      ]
    : [path.join(repoRoot, 'desktop', 'assets', 'workass-macos.png')];
  return candidates.find((candidate) => fs.existsSync(candidate)) || null;
}

function resolveWindowIconPath({ platform = process.platform, isPackaged, resourcesPath, repoRoot }) {
  if (platform !== 'win32') return null;
  const candidates = isPackaged
    ? [path.join(resourcesPath, 'Workass.ico')]
    : [path.join(repoRoot, 'desktop', 'assets', 'icon.ico')];
  return candidates.find((candidate) => fs.existsSync(candidate)) || null;
}

function applyMacDockIcon({ app, nativeImage, isPackaged, resourcesPath, repoRoot, platform = process.platform }) {
  if (platform !== 'darwin' || !app || !app.dock || typeof app.dock.setIcon !== 'function') {
    return { applied: false, reason: 'unsupported-platform' };
  }
  const iconPath = resolveAppIconPath({ isPackaged, resourcesPath, repoRoot });
  if (!iconPath) return { applied: false, reason: 'icon-missing' };
  const icon = nativeImage.createFromPath(iconPath);
  if (!icon || icon.isEmpty()) return { applied: false, reason: 'icon-empty', iconPath };
  app.dock.setIcon(icon);
  return { applied: true, iconPath };
}

module.exports = { applyMacDockIcon, resolveAppIconPath, resolveWindowIconPath };
