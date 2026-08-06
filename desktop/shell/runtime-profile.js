'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const ALLOWED = new Set([
  'WORKASS_PROFILE', 'WORKASS_APP_NAME', 'WORKASS_BUNDLE_ID',
  'WORKASS_DAEMON_PORT', 'WORKASS_DAEMON_BIND', 'WORKASS_VIEW_PORT',
  'WORKASS_LAUNCHD_LABEL', 'WORKASS_DATA_ROOT', 'WORKASS_LOG_ROOT',
]);

function expandValue(value, vars) {
  return value.replace(/\$\{([A-Z0-9_]+)\}/g, (_match, name) => {
    if (!Object.prototype.hasOwnProperty.call(vars, name) || vars[name] == null || vars[name] === '') {
      throw new Error(`profile variable ${name} is not defined`);
    }
    return String(vars[name]);
  });
}

function parseProfile(text, seed = {}) {
  const values = { ...seed };
  for (const [index, raw] of String(text).split(/\r?\n/).entries()) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const match = line.match(/^([A-Z][A-Z0-9_]*)=(.*)$/);
    if (!match || !ALLOWED.has(match[1])) throw new Error(`invalid profile assignment on line ${index + 1}`);
    let value = match[2].trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    if (/`|\$\(|;|&&|\|\|/.test(value)) throw new Error(`unsafe profile value on line ${index + 1}`);
    values[match[1]] = expandValue(value, values);
  }
  return values;
}

function numericPort(value, name, allowZero = false) {
  const port = Number(value);
  if (!Number.isInteger(port) || port < (allowZero ? 0 : 1) || port > 65535) {
    throw new Error(`${name} must be ${allowZero ? '0-' : '1-'}65535`);
  }
  return port;
}

function resolveRuntimeProfile({ env = process.env, isPackaged = false, resourcesPath = '', repoRoot = '' } = {}) {
  const profile = String(env.WORKASS_PROFILE || (isPackaged ? 'prod' : 'dev')).trim();
  if (!['prod', 'dev', 'test'].includes(profile)) throw new Error(`unknown Workass profile: ${profile}`);
  const profileFile = env.WORKASS_PROFILE_FILE || (isPackaged
    ? path.join(resourcesPath, 'workass-profile.env')
    : path.join(repoRoot, 'config', 'environments', `${profile}.env`));
  const home = env.HOME || env.USERPROFILE || os.homedir();
  const seed = {
    HOME: home,
    USERPROFILE: env.USERPROFILE || home,
    LOCALAPPDATA: env.LOCALAPPDATA || path.join(home, 'AppData', 'Local'),
    WORKASS_REPO_ROOT: repoRoot,
    WORKASS_TEST_ROOT: env.WORKASS_TEST_ROOT,
  };
  const fileValues = parseProfile(fs.readFileSync(profileFile, 'utf8'), seed);
  if (fileValues.WORKASS_PROFILE !== profile) throw new Error(`profile identity mismatch in ${profileFile}`);

  const values = { ...fileValues };
  for (const key of ALLOWED) if (env[key] != null && env[key] !== '') values[key] = String(env[key]);
  const dataRoot = path.resolve(values.WORKASS_DATA_ROOT);
  const daemonPort = numericPort(values.WORKASS_DAEMON_PORT, 'WORKASS_DAEMON_PORT', profile === 'test');
  const viewPort = numericPort(values.WORKASS_VIEW_PORT, 'WORKASS_VIEW_PORT', profile === 'test');
  if (!['localhost', 'lan'].includes(values.WORKASS_DAEMON_BIND)) throw new Error('WORKASS_DAEMON_BIND must be localhost or lan');

  return {
    profile,
    appName: values.WORKASS_APP_NAME,
    bundleId: values.WORKASS_BUNDLE_ID,
    daemonPort,
    daemonBind: values.WORKASS_DAEMON_BIND,
    daemonURL: env.WORKASS_URL || `https://127.0.0.1:${daemonPort}`,
    launchdLabel: values.WORKASS_LAUNCHD_LABEL,
    viewPort: env.WORKASS_VIEW_PORT != null && env.WORKASS_VIEW_PORT !== ''
      ? numericPort(env.WORKASS_VIEW_PORT, 'WORKASS_VIEW_PORT', profile === 'test') : viewPort,
    dataRoot,
    stateDir: path.join(dataRoot, 'state'),
    userDataDir: path.join(dataRoot, 'electron'),
    runDir: path.join(dataRoot, 'run'),
    browserControlFile: env.WORKASS_BROWSER_CONTROL_FILE || path.join(dataRoot, 'run', 'browser-control.json'),
    logRoot: path.resolve(values.WORKASS_LOG_ROOT),
    profileFile,
  };
}

module.exports = { parseProfile, resolveRuntimeProfile };
