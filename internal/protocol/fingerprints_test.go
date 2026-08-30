package protocol

import (
	"reflect"
	"testing"
)

func TestRuntimeFingerprintsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		fp   RuntimeFingerprints
		want []string
	}{
		{name: "empty", want: []string{"build", "actionCatalog", "schema"}},
		{name: "partial", fp: RuntimeFingerprints{Build: "abc", Schema: "schema"}, want: []string{"actionCatalog"}},
		{name: "complete", fp: RuntimeFingerprints{Build: "abc", ActionCatalog: "actions", Schema: "schema"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fp.MissingFields(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MissingFields() = %v, want %v", got, tc.want)
			}
		})
	}
}
