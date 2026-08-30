/// <reference types="@jlceda/pro-api-types" />

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { fingerprintMismatches, missingFingerprintFields } from './runtime-fingerprints';

test('mutation identity requires every runtime fingerprint field', () => {
	assert.deepEqual(missingFingerprintFields(undefined), ['build', 'actionCatalog', 'schema']);
	assert.deepEqual(missingFingerprintFields({ build: 'abc', schema: 'schema' }), ['actionCatalog']);
	assert.deepEqual(missingFingerprintFields({
		build: 'abc', actionCatalog: 'actions', schema: 'schema',
	}), []);
});

test('explicit runtime fingerprint differences remain distinguishable from missing fields', () => {
	const daemon = { build: 'abc', actionCatalog: 'actions-a', schema: 'schema' };
	const connector = { build: 'abc', actionCatalog: 'actions-b', schema: 'schema' };
	assert.deepEqual(fingerprintMismatches(daemon, connector), ['actionCatalog']);
});
