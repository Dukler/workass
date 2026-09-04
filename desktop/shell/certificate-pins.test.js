'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { createHash } = require('node:crypto');
const {
  CertificatePins,
  certificateVerificationSessions,
  fingerprintCertificateData,
  normalizeCertFingerprint,
  privateLANHost,
  trustCertificatePEM,
} = require('./certificate-pins');

const fingerprint = 'ab'.repeat(32);

test('private Workass endpoint is accepted only under its exact SHA-256 certificate pin', () => {
  const pins = new CertificatePins();
  assert.equal(pins.trustEndpoint('192.168.1.71:80', fingerprint), true);
  assert.equal(pins.verify({
    hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_COMMON_NAME_INVALID',
	certificate: { fingerprint: fingerprint.toUpperCase().match(/../g).join(':') },
  }), true);
	assert.equal(pins.verify({
		hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		certificate: {
			fingerprint: '01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67',
			fingerprint256: fingerprint.toUpperCase().match(/../g).join(':'),
		},
	}), true, 'Electron legacy fingerprint must not mask its matching SHA-256 pin');
	assert.deepEqual(pins.decision({
		hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		certificate: { fingerprint256: 'cd'.repeat(32) },
	}), {
		accepted: false,
		host: '192.168.1.71',
		verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		privateLAN: true,
		pinKnown: true,
		fingerprint256Valid: true,
		fingerprintValid: false,
		certificateDataValid: false,
		fingerprintMatches: false,
	});
  assert.equal(pins.verify({
    hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
    certificate: { fingerprint256: 'cd'.repeat(32) },
  }), false);
});

test('the documented PEM certificate data yields the daemon SHA-256 identity', () => {
	const der = Buffer.from([1, 2, 3, 4, 5, 6]);
	const pem = `-----BEGIN CERTIFICATE-----\n${der.toString('base64')}\n-----END CERTIFICATE-----`;
	const expected = createHash('sha256').update(der).digest('hex');
	assert.equal(fingerprintCertificateData(pem), expected);

	const pins = new CertificatePins();
	assert.equal(pins.trustEndpoint('192.168.1.71:80', expected), true);
	assert.equal(pins.verify({
		hostname: '192.168.1.71',
		verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		certificate: { fingerprint: '01:23:45', data: pem },
	}), true);
	assert.equal(fingerprintCertificateData('not a certificate'), '');
});

test('local daemon PEM establishes the exact loopback pin', () => {
	const der = Buffer.from([7, 8, 9, 10]);
	const pem = `-----BEGIN CERTIFICATE-----\n${der.toString('base64')}\n-----END CERTIFICATE-----`;
	const pins = new CertificatePins();
	assert.equal(trustCertificatePEM(pins, '127.0.0.1:8788', pem), true);
	assert.equal(pins.verify({
		hostname: '127.0.0.1',
		verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		certificate: { data: pem },
	}), true);
	assert.equal(trustCertificatePEM(pins, '127.0.0.1:8788', 'not a certificate'), false);
});

test('certificate verification covers the renderer and isolated in-app browser sessions', () => {
	const defaultSession = { name: 'renderer' };
	const browserSession = { name: 'browser' };
	const calls = [];
	const sessions = certificateVerificationSessions({
		defaultSession,
		fromPartition(partition, options) {
			calls.push([partition, options]);
			return browserSession;
		},
	}, 'persist:workass-browser');
	assert.deepEqual(calls, [['persist:workass-browser', { cache: true }]]);
	assert.deepEqual(sessions, [defaultSession, browserSession]);

	assert.deepEqual(certificateVerificationSessions({
		defaultSession,
		fromPartition: () => defaultSession,
	}, 'persist:workass-browser'), [defaultSession], 'the same Electron session is configured only once');
});

test('pins never relax public hosts, unrelated TLS failures, or malformed fingerprints', () => {
  const pins = new CertificatePins();
  assert.equal(pins.trustEndpoint('example.com:443', fingerprint), false);
  assert.equal(pins.trustEndpoint('192.168.1.71:80', 'not-a-fingerprint'), false);
  assert.equal(pins.verify({ hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_DATE_INVALID', certificate: { fingerprint256: fingerprint } }), false);
  assert.equal(privateLANHost('172.31.9.2'), true);
  assert.equal(privateLANHost('172.32.9.2'), false);
  assert.equal(normalizeCertFingerprint(fingerprint.toUpperCase().match(/../g).join(':')), fingerprint);
});
