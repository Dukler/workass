'use strict';

const net = require('node:net');
const crypto = require('node:crypto');

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

class CertificatePins {
  constructor() {
    this.byHost = new Map();
  }

  trustEndpoint(address, fingerprint) {
    let parsed;
    try { parsed = new URL(`https://${String(address || '').trim()}`); } catch { return false; }
    const host = parsed.hostname.replace(/^\[|\]$/g, '');
    const pin = normalizeCertFingerprint(fingerprint);
    if (!privateLANHost(host) || !pin) return false;
    this.byHost.set(host, pin);
    return true;
  }

  decision(request) {
    const host = String(request?.hostname || '').replace(/^\[|\]$/g, '');
    const expected = this.byHost.get(host);
	// Electron exposes both its legacy certificate fingerprint and the SHA-256
	// fingerprint on some platforms.  The legacy value is truthy but too short,
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
	return {
		accepted: isPrivate && expectedSelfSignedFailure && !!expected && actual === expected,
		host,
		verificationResult: result,
		privateLAN: isPrivate,
		pinKnown: !!expected,
		fingerprint256Valid: !!fingerprint256,
		fingerprintValid: !!fingerprint,
		certificateDataValid: !!fingerprintData,
		fingerprintMatches: !!expected && actual === expected,
	};
  }

  verify(request) {
	return this.decision(request).accepted;
  }
}

module.exports = { CertificatePins, fingerprintCertificateData, normalizeCertFingerprint, privateLANHost };
