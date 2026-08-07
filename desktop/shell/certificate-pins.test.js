'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { CertificatePins, normalizeCertFingerprint, privateLANHost } = require('./certificate-pins');

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
    certificate: { fingerprint256: 'cd'.repeat(32) },
  }), false);
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
