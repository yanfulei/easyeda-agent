package protocol

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zhoushoujianwork/easyeda-agent/internal/version"
)

// RuntimeFingerprints identify the exact executable/extension build, typed
// action surface, and wire schema. Version strings alone cannot distinguish a
// locally fixed daemon from a stale connector carrying the same release number.
type RuntimeFingerprints struct {
	Build         string `json:"build,omitempty"`
	ActionCatalog string `json:"actionCatalog,omitempty"`
	Schema        string `json:"schema,omitempty"`
}

func (f RuntimeFingerprints) Empty() bool {
	return f.Build == "" && f.ActionCatalog == "" && f.Schema == ""
}

// MissingFields reports incomplete runtime identity. Mutation admission needs
// every field: comparing only the fields an old connector happens to send can
// turn an unknown compatibility state into a false match.
func (f RuntimeFingerprints) MissingFields() []string {
	var missing []string
	if strings.TrimSpace(f.Build) == "" {
		missing = append(missing, "build")
	}
	if strings.TrimSpace(f.ActionCatalog) == "" {
		missing = append(missing, "actionCatalog")
	}
	if strings.TrimSpace(f.Schema) == "" {
		missing = append(missing, "schema")
	}
	return missing
}

// wireSchemaDescriptor is deliberately mirrored in
// extension/src/runtime-fingerprints.ts. Updating a covered wire field requires
// updating the frozen cross-language hash tests as an explicit compatibility
// decision.
const wireSchemaDescriptor = `easyeda-agent-wire-schema-v3
handshake:fingerprints{actionCatalog,build,schema},service,type,version
register:capabilities,connectorVersion,easyedaVersion,fingerprints{actionCatalog,build,schema},type,windowId
request:action,createdAt,expectedContext{documentType,documentUuid,projectUuid},id,mutates,payload,timeoutMs,type,version,windowId,writeSensitive
response:abandonedIds,artifacts,context{documentType,documentUuid,projectName,projectUuid,tabId,unit},error{code,detail,message,retryable,uncertain},id,ok,result,seq,seqAbandoned,type,unordered,version,warnings`

var (
	actionCatalogFingerprint = sync.OnceValue(func() string {
		names := make([]string, 0, len(AllActions()))
		for _, action := range AllActions() {
			if action.NeedsWindow {
				names = append(names, action.Name)
			}
		}
		sort.Strings(names)
		return fnv1a32(strings.Join(names, "\n"))
	})
	schemaFingerprint = sync.OnceValue(func() string {
		return fnv1a32(wireSchemaDescriptor)
	})
)

func CurrentRuntimeFingerprints() RuntimeFingerprints {
	return RuntimeFingerprints{
		Build:         version.BuildFingerprint,
		ActionCatalog: actionCatalogFingerprint(),
		Schema:        schemaFingerprint(),
	}
}

// FingerprintMismatches reports only explicit, comparable differences. Missing
// values and development build sentinels are unknown, preserving read/health
// diagnostics while an older peer is being upgraded.
func FingerprintMismatches(daemon, connector RuntimeFingerprints) []string {
	var mismatches []string
	if comparableBuild(daemon.Build) && comparableBuild(connector.Build) && daemon.Build != connector.Build {
		mismatches = append(mismatches, "build")
	}
	if daemon.ActionCatalog != "" && connector.ActionCatalog != "" && daemon.ActionCatalog != connector.ActionCatalog {
		mismatches = append(mismatches, "actionCatalog")
	}
	if daemon.Schema != "" && connector.Schema != "" && daemon.Schema != connector.Schema {
		mismatches = append(mismatches, "schema")
	}
	return mismatches
}

// FingerprintMatch returns nil when the peers share no comparable field, false
// on any explicit mismatch, and true when at least one field is comparable and
// every comparable field matches.
func FingerprintMatch(daemon, connector RuntimeFingerprints) *bool {
	if len(FingerprintMismatches(daemon, connector)) > 0 {
		matched := false
		return &matched
	}
	comparable := (comparableBuild(daemon.Build) && comparableBuild(connector.Build)) ||
		(daemon.ActionCatalog != "" && connector.ActionCatalog != "") ||
		(daemon.Schema != "" && connector.Schema != "")
	if !comparable {
		return nil
	}
	matched := true
	return &matched
}

func comparableBuild(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v != "" && v != "dev" && !strings.HasSuffix(v, "-dev")
}

func fnv1a32(value string) string {
	const prime32 = uint32(16777619)
	hash := uint32(2166136261)
	for i := 0; i < len(value); i++ {
		hash ^= uint32(value[i])
		hash *= prime32
	}
	return fmt.Sprintf("fnv1a32:%08x", hash)
}
