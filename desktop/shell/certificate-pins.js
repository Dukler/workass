'use strict';

const net = require('node:net');
const crypto = require('node:crypto');

// Two rules, one per kind of Electron session (portable-networking §5).
//
// The app's own window reaches the owner's machines over whatever network they
// chose, a VPN included, so a pin there holds at any address class. The agent
// browser holds an agent's site sign-ins: a book entry naming a public host must
// never relax TLS for that site, so a pin there holds only at a private address.
const APP_CERTIFICATE_POLICY = 'app';
const BROWSER_CERTIFICATE_POLICY = 'browser';

// The agent browser's rule, exactly as it was before the app window had its own.
function privateLANHost(hostname) {
  const host = String(hostname || '').replace(/^\[|\]$/g, '');
  if (host === 'localhost' || host === '::1') return true;
  if (net.isIP(host) !== 4) return false;
  const [a, b] = host.split('.').map(Number);
  return a === 10 || a === 127 || (a === 192 && b === 168) || (a === 172 && b >= 16 && b <= 31);
}

function normalizeCertFingerprint(value) {
  const normalized = String(value || '').replaceAll(':', '').trim().toLowerCase();
  return /^[a-f0-9]{64}$/.test(normalized) ? normalized : '';
}

function fingerprintCertificateData(value) {
	const match = String(value || '').match(/-----BEGIN CERTIFICATE-----([\s\S]+?)-----END CERTIFICATE-----/);
	if (!match) return '';
	const encoded = match[1].replace(/\s+/g, '');
	if (!encoded || !/^[A-Za-z0-9+/]+={0,2}$/.test(encoded)) return '';
	try {
		const der = Buffer.from(encoded, 'base64');
		return der.length ? crypto.createHash('sha256').update(der).digest('hex') : '';
	} catch {
		return '';
	}
}

function trustCertificatePEM(pins, address, certificateData, owner = 'local-daemon') {
	if (!pins || typeof pins.trustEndpoint !== 'function') return false;
	const fingerprint = fingerprintCertificateData(certificateData);
	return !!fingerprint && pins.trustEndpoint(address, fingerprint, owner);
}

// Electron certificate verification is scoped to a Session. The renderer uses
// defaultSession, while every in-app browser view uses its own persistent
// partition. Installing a verifier on only the former leaves browser.open
// unable to load the same pinned Workass HTTPS endpoint the renderer can use.
// Each session comes back with the rule its verifier applies; a session the
// agent browser shares takes the browser's.
function certificateVerificationSessions(sessionModule, browserPartition) {
	if (!sessionModule || !sessionModule.defaultSession || typeof sessionModule.fromPartition !== 'function') {
		return [];
	}
	const app = sessionModule.defaultSession;
	const browser = sessionModule.fromPartition(browserPartition, { cache: true });
	if (browser === app) return [{ target: app, policy: BROWSER_CERTIFICATE_POLICY }];
	const targets = [{ target: app, policy: APP_CERTIFICATE_POLICY }];
	if (browser) targets.push({ target: browser, policy: BROWSER_CERTIFICATE_POLICY });
	return targets;
}

// One spelling per host, the one Chromium hands the verifier: lower case, and an
// IPv6 address compressed and without brackets.
function canonicalHost(hostname) {
	let host = String(hostname || '').trim();
	if (!host) return '';
	if (host.includes(':') && !host.startsWith('[')) host = `[${host}]`;
	try {
		return new URL(`https://${host}`).hostname.replace(/^\[|\]$/g, '');
	} catch {
		return '';
	}
}

function endpointHost(address) {
	try {
		return new URL(`https://${String(address || '').trim()}`).hostname.replace(/^\[|\]$/g, '');
	} catch {
		return '';
	}
}

class CertificatePins {
  constructor() {
    this.byHost = new Map();
  }

  // Chromium asks about a hostname, with no port and no idea which machine a socket
  // meant, so one host has one pin. A second machine at that host must not replace
  // it: the first machine's reconnect would then hand its device token to whoever
  // holds the new certificate. The owner may change its own pin once it is proved.
  // Any address class can hold a pin; which sessions honour it is the verifier's rule.
  trustEndpoint(address, fingerprint, owner = '') {
    const host = endpointHost(address);
    const pin = normalizeCertFingerprint(fingerprint);
    if (!host || !pin) return false;
    const held = this.byHost.get(host);
    if (held && held.pin !== pin && held.owner !== String(owner)) return false;
    this.byHost.set(host, { pin, owner: String(owner) });
    return true;
  }

  // A reload loses the sockets these pins were for. Without this, a pin taken by a
  // forger would block the real machine until the app was relaunched.
  releaseAllExcept(owner) {
    for (const [host, held] of this.byHost) {
      if (held.owner !== owner) this.byHost.delete(host);
    }
  }

  releaseEndpoint(address, owner = '') {
    const host = endpointHost(address);
    const held = host && this.byHost.get(host);
    if (held && held.owner === String(owner)) this.byHost.delete(host);
  }

  // A caller that names no rule gets the agent browser's, the stricter one.
  decision(request, policy = BROWSER_CERTIFICATE_POLICY) {
    const rule = policy === APP_CERTIFICATE_POLICY ? APP_CERTIFICATE_POLICY : BROWSER_CERTIFICATE_POLICY;
    const host = canonicalHost(request?.hostname);
    const expected = host ? this.byHost.get(host)?.pin : undefined;
	// Electron exposes both its short certificate fingerprint and the SHA-256
	// fingerprint on some platforms. The short value is truthy but too small,
	// so selecting it with `||` discarded the valid fingerprint256 and made every
	// pinned Workass certificate fail with ERR_CERT_AUTHORITY_INVALID.
	const fingerprint256 = normalizeCertFingerprint(request?.certificate?.fingerprint256);
	const fingerprint = normalizeCertFingerprint(request?.certificate?.fingerprint);
	// Electron's documented cross-platform identity is the PEM `data` field.
	// Current macOS Electron returns a non-SHA-256 `fingerprint` and no
	// `fingerprint256`, so derive the same SHA-256-over-DER identity the daemon
	// advertises when neither convenience field is usable.
	const fingerprintData = fingerprintCertificateData(request?.certificate?.data);
	const actual = fingerprint256 || fingerprint || fingerprintData;
	const result = String(request?.verificationResult || '');
	const expectedSelfSignedFailure = result.endsWith('ERR_CERT_AUTHORITY_INVALID')
		|| result.endsWith('ERR_CERT_COMMON_NAME_INVALID');
	const isPrivate = privateLANHost(host);
	const addressAllowed = rule === APP_CERTIFICATE_POLICY || isPrivate;
	return {
		accepted: addressAllowed && expectedSelfSignedFailure && !!expected && actual === expected,
		policy: rule,
		host,
		verificationResult: result,
		privateLAN: isPrivate,
		addressAllowed,
		pinKnown: !!expected,
		fingerprint256Valid: !!fingerprint256,
		fingerprintValid: !!fingerprint,
		certificateDataValid: !!fingerprintData,
		fingerprintMatches: !!expected && actual === expected,
	};
  }

  verify(request, policy) {
	return this.decision(request, policy).accepted;
  }
}

module.exports = {
	APP_CERTIFICATE_POLICY,
	BROWSER_CERTIFICATE_POLICY,
	CertificatePins,
	certificateVerificationSessions,
	fingerprintCertificateData,
	normalizeCertFingerprint,
	privateLANHost,
	trustCertificatePEM,
};
