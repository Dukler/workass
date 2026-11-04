'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { createHash } = require('node:crypto');
const {
  APP_CERTIFICATE_POLICY,
  BROWSER_CERTIFICATE_POLICY,
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
		policy: BROWSER_CERTIFICATE_POLICY,
		host: '192.168.1.71',
		verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID',
		privateLAN: true,
		addressAllowed: true,
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

test('certificate verification covers the renderer and isolated in-app browser sessions, each under its own rule', () => {
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
	assert.deepEqual(sessions, [
		{ target: defaultSession, policy: APP_CERTIFICATE_POLICY },
		{ target: browserSession, policy: BROWSER_CERTIFICATE_POLICY },
	]);

	assert.deepEqual(certificateVerificationSessions({
		defaultSession,
		fromPartition: () => defaultSession,
	}, 'persist:workass-browser'), [{ target: defaultSession, policy: BROWSER_CERTIFICATE_POLICY }],
	'a session the agent browser shares is configured once, under the browser rule');
});

test('a second machine at the same host cannot take over the pin', () => {
  const pins = new CertificatePins();
  const real = 'ab'.repeat(32);
  const forged = 'cd'.repeat(32);
  assert.equal(pins.trustEndpoint('192.168.1.71:80', real, 'm-real'), true);
  assert.equal(pins.trustEndpoint('192.168.1.71:9000', forged, 'm-forged'), false, 'another port is the same host to Chromium');
  const verdict = (fingerprint256) => pins.verify({
    hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_AUTHORITY_INVALID', certificate: { fingerprint256 },
  });
  assert.equal(verdict(real), true);
  assert.equal(verdict(forged), false);
  pins.releaseEndpoint('192.168.1.71:80', 'm-forged');
  assert.equal(verdict(real), true, 'only the holder can release it');
  assert.equal(pins.trustEndpoint('192.168.1.71:80', forged, 'm-real'), true, 'the holder can change its own pin');
  pins.releaseEndpoint('192.168.1.71:80', 'm-real');
  assert.equal(pins.trustEndpoint('192.168.1.71:80', real, 'm-new'), true, 'a released host is free again');
});

test('a renderer reload frees remote pins and keeps the local daemon pin', () => {
  const pins = new CertificatePins();
  const local = 'ab'.repeat(32);
  const remote = 'cd'.repeat(32);
  assert.equal(pins.trustEndpoint('127.0.0.1:8788', local, 'local-daemon'), true);
  assert.equal(pins.trustEndpoint('192.168.1.71:80', remote, 'm-forged'), true);
  pins.releaseAllExcept('local-daemon');
  assert.equal(pins.trustEndpoint('192.168.1.71:80', local, 'm-real'), true, 'the real machine can pin again');
  assert.equal(pins.trustEndpoint('127.0.0.1:8788', remote, 'm-other'), false, 'the local daemon stays pinned');
});

const pinned = (hostname, fingerprint256, verificationResult = 'net::ERR_CERT_AUTHORITY_INVALID') => ({
  hostname, verificationResult, certificate: { fingerprint256 },
});

test('the app window reaches a pinned machine at a VPN address, an IPv6 address or a hostname', () => {
  const pins = new CertificatePins();
  const app = (request) => pins.verify(request, APP_CERTIFICATE_POLICY);
  assert.equal(pins.trustEndpoint('100.64.0.7:8788', fingerprint, 'm-vpn'), true, 'an overlay VPN address can be pinned');
  assert.equal(app(pinned('100.64.0.7', fingerprint)), true);
  assert.equal(app(pinned('100.64.0.7', fingerprint, 'net::ERR_CERT_COMMON_NAME_INVALID')), true);
  assert.equal(app(pinned('100.64.0.7', 'cd'.repeat(32))), false, 'only the exact pinned certificate');

  const v6 = 'cd'.repeat(32);
  assert.equal(pins.trustEndpoint('[FD7A:115C:A1E0:0:0:0:0:1]:8788', v6, 'm-v6'), true, 'an IPv6 address in brackets');
  assert.equal(app(pinned('fd7a:115c:a1e0::1', v6)), true, 'Chromium names an IPv6 host compressed and without brackets');
  assert.equal(app(pinned('[fd7a:115c:a1e0::1]', v6)), true);
  assert.equal(app(pinned('fd7a:115c:a1e0::2', v6)), false, 'a neighbour address holds no pin');

  const named = 'ef'.repeat(32);
  assert.equal(pins.trustEndpoint('Builder.Tailnet.Example:8788', named, 'm-named'), true, 'a hostname');
  assert.equal(app(pinned('builder.tailnet.example', named)), true);
  assert.equal(app(pinned('builder.tailnet.example', fingerprint)), false);
  assert.equal(pins.trustEndpoint('builder.tailnet.example:9000', fingerprint, 'm-other'), false,
    'the owner rule holds for a hostname: another machine cannot take over its pin');
});

test('a pinned machine never relaxes TLS for a website in the agent browser', () => {
  const pins = new CertificatePins();
  const browser = (request) => pins.verify(request, BROWSER_CERTIFICATE_POLICY);
  // The book can name any host, a public one included. The agent browser holds an
  // agent's site sign-ins, so a book entry there must change nothing.
  assert.equal(pins.trustEndpoint('example.com:443', fingerprint, 'm-public'), true);
  assert.equal(pins.trustEndpoint('93.184.215.14:443', fingerprint, 'm-public-ip'), true);
  assert.equal(pins.trustEndpoint('100.64.0.7:8788', fingerprint, 'm-vpn'), true);
  assert.equal(pins.trustEndpoint('[fd7a:115c:a1e0::1]:8788', fingerprint, 'm-v6'), true);
  for (const host of ['example.com', '93.184.215.14', '100.64.0.7', 'fd7a:115c:a1e0::1']) {
    assert.equal(browser(pinned(host, fingerprint)), false, `${host} stays refused in the agent browser`);
    assert.equal(pins.verify(pinned(host, fingerprint)), false, `${host}: a caller naming no rule gets the strict one`);
    assert.equal(pins.verify(pinned(host, fingerprint), APP_CERTIFICATE_POLICY), true, `${host}: the app window reaches its machine`);
  }
  assert.equal(pins.decision(pinned('example.com', fingerprint), BROWSER_CERTIFICATE_POLICY).addressAllowed, false);
  assert.equal(pins.trustEndpoint('192.168.1.71:80', fingerprint, 'm-lan'), true);
  assert.equal(browser(pinned('192.168.1.71', fingerprint)), true, 'a private machine still opens in the agent browser');
});

test('the app window forgives only the self-signed authority and name errors', () => {
  const pins = new CertificatePins();
  assert.equal(pins.trustEndpoint('100.64.0.7:8788', fingerprint, 'm-vpn'), true);
  for (const result of ['net::ERR_CERT_DATE_INVALID', 'net::ERR_CERT_REVOKED', 'net::ERR_CERT_INVALID', '']) {
    assert.equal(pins.verify(pinned('100.64.0.7', fingerprint, result), APP_CERTIFICATE_POLICY), false, result || 'no result');
  }
});

test('pins never take an unusable host, unrelated TLS failures, or malformed fingerprints', () => {
  const pins = new CertificatePins();
  assert.equal(pins.trustEndpoint('', fingerprint), false);
  assert.equal(pins.trustEndpoint('fd7a::1:8788', fingerprint), false, 'an IPv6 address without brackets is ambiguous');
  assert.equal(pins.trustEndpoint('192.168.1.71:80', 'not-a-fingerprint'), false);
  assert.equal(pins.verify({ hostname: '192.168.1.71', verificationResult: 'net::ERR_CERT_DATE_INVALID', certificate: { fingerprint256: fingerprint } }), false);
  assert.equal(privateLANHost('172.31.9.2'), true);
  assert.equal(privateLANHost('172.32.9.2'), false);
  assert.equal(privateLANHost('100.64.0.7'), false, 'the agent browser rule stays exactly as it was');
  assert.equal(normalizeCertFingerprint(fingerprint.toUpperCase().match(/../g).join(':')), fingerprint);
});
