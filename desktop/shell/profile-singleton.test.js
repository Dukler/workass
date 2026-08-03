'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const { acquireProfileSingleton } = require('./profile-singleton');

class FakeApp extends EventEmitter {
  constructor(acquired) {
    super();
    this.acquired = acquired;
    this.request = null;
  }

  requestSingleInstanceLock(request) {
    this.request = request;
    return this.acquired;
  }
}

test('a duplicate shell is rejected for the exact runtime profile', () => {
  const app = new FakeApp(false);
  const acquired = acquireProfileSingleton({
    app,
    profile: 'dev',
    dataRoot: '/tmp/workass-dev',
    getWindows: () => [],
  });

  assert.equal(acquired, false);
  assert.deepEqual(app.request, { workassProfile: 'dev', workassDataRoot: '/tmp/workass-dev' });
  assert.equal(app.listenerCount('second-instance'), 0);
});

test('a second launch restores and focuses the existing profile window', () => {
  const calls = [];
  const win = {
    isDestroyed: () => false,
    isMinimized: () => true,
    restore: () => calls.push('restore'),
    show: () => calls.push('show'),
    focus: () => calls.push('focus'),
  };
  const app = new FakeApp(true);
  const acquired = acquireProfileSingleton({
    app,
    profile: 'dev',
    dataRoot: '/tmp/workass-dev',
    getWindows: () => [win],
  });

  assert.equal(acquired, true);
  app.emit('second-instance');
  assert.deepEqual(calls, ['restore', 'show', 'focus']);
});
