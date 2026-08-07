'use strict';

const net = require('node:net');

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

  verify(request) {
    const host = String(request?.hostname || '').replace(/^\[|\]$/g, '');
    const expected = this.byHost.get(host);
	const actual = normalizeCertFingerprint(request?.certificate?.fingerprint || request?.certificate?.fingerprint256);
	const result = String(request?.verificationResult || '');
	const expectedSelfSignedFailure = result.endsWith('ERR_CERT_AUTHORITY_INVALID')
		|| result.endsWith('ERR_CERT_COMMON_NAME_INVALID');
    return privateLANHost(host) && expectedSelfSignedFailure && !!expected && actual === expected;
  }
}

module.exports = { CertificatePins, normalizeCertFingerprint, privateLANHost };
