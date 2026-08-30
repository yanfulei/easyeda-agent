import { connectorActionNames } from './actions';
import { BUILD_FINGERPRINT, type RuntimeFingerprints } from './protocol';

// Mirrored by internal/protocol/fingerprints.go. The frozen hash tests on both
// sides make a wire change an explicit compatibility decision.
export const WIRE_SCHEMA_DESCRIPTOR = `easyeda-agent-wire-schema-v3
handshake:fingerprints{actionCatalog,build,schema},service,type,version
register:capabilities,connectorVersion,easyedaVersion,fingerprints{actionCatalog,build,schema},type,windowId
request:action,createdAt,expectedContext{documentType,documentUuid,projectUuid},id,mutates,payload,timeoutMs,type,version,windowId,writeSensitive
response:abandonedIds,artifacts,context{documentType,documentUuid,projectName,projectUuid,tabId,unit},error{code,detail,message,retryable,uncertain},id,ok,result,seq,seqAbandoned,type,unordered,version,warnings`;

export function fnv1a32(value: string): string {
	let hash = 0x811c9dc5;
	for (let i = 0; i < value.length; i += 1) {
		hash ^= value.charCodeAt(i) & 0xff;
		hash = Math.imul(hash, 0x01000193) >>> 0;
	}
	return `fnv1a32:${hash.toString(16).padStart(8, '0')}`;
}

export function currentRuntimeFingerprints(): RuntimeFingerprints {
	return {
		build: BUILD_FINGERPRINT,
		actionCatalog: fnv1a32(connectorActionNames().join('\n')),
		schema: fnv1a32(WIRE_SCHEMA_DESCRIPTOR),
	};
}

export function missingFingerprintFields(
	fingerprints: RuntimeFingerprints | undefined,
): Array<keyof RuntimeFingerprints> {
	const missing: Array<keyof RuntimeFingerprints> = [];
	if (!fingerprints?.build?.trim()) missing.push('build');
	if (!fingerprints?.actionCatalog?.trim()) missing.push('actionCatalog');
	if (!fingerprints?.schema?.trim()) missing.push('schema');
	return missing;
}

export function fingerprintMismatches(
	daemon: RuntimeFingerprints | undefined,
	connector: RuntimeFingerprints,
): Array<keyof RuntimeFingerprints> {
	if (!daemon) return [];
	const mismatches: Array<keyof RuntimeFingerprints> = [];
	if (comparableBuild(daemon.build) && comparableBuild(connector.build)
		&& daemon.build !== connector.build) {
		mismatches.push('build');
	}
	if (daemon.actionCatalog && connector.actionCatalog
		&& daemon.actionCatalog !== connector.actionCatalog) {
		mismatches.push('actionCatalog');
	}
	if (daemon.schema && connector.schema && daemon.schema !== connector.schema) {
		mismatches.push('schema');
	}
	return mismatches;
}

function comparableBuild(value: string | undefined): value is string {
	const normalized = value?.trim().toLowerCase() ?? '';
	return normalized !== '' && normalized !== 'dev' && !normalized.endsWith('-dev');
}
