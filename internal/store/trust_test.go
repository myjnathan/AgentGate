package store

import (
	"encoding/json"
	"testing"
)

func TestParseTrustType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want TrustType
		ok   bool
	}{
		{"signed_cloud", TrustSignedCloud, true},
		{"dedicated_cloud", TrustDedicatedCloud, true},
		{"local_model", TrustLocalModel, true},
		{"", "", false},
		{"private", "", false},
		{"SIGNED_CLOUD", "", false},
	} {
		got, ok := ParseTrustType(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ParseTrustType(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTrustTypeJSONRoundTrip(t *testing.T) {
	for _, tt := range []TrustType{TrustSignedCloud, TrustDedicatedCloud, TrustLocalModel} {
		b, err := json.Marshal(tt)
		if err != nil {
			t.Fatal(err)
		}
		var back TrustType
		if err := json.Unmarshal(b, &back); err != nil || back != tt {
			t.Fatalf("round-trip %q -> %q (%v)", tt, back, err)
		}
	}
	// An explicit empty string normalizes to signed cloud.
	var back TrustType
	if err := json.Unmarshal([]byte(`""`), &back); err != nil || back != TrustSignedCloud {
		t.Fatalf("empty trust type should normalize to signed_cloud, got %q (%v)", back, err)
	}
	// An unknown value is rejected, not silently accepted.
	if err := json.Unmarshal([]byte(`"onprem"`), &back); err == nil {
		t.Fatal("unknown trust type should fail to unmarshal")
	}
}

func TestChannelTrustTypeOrDefault(t *testing.T) {
	// Channels stored before F17 have no trust type: they behave as
	// signed cloud, the previous default.
	ch := &Channel{}
	if got := ch.TrustTypeOrDefault(); got != TrustSignedCloud {
		t.Fatalf("empty trust type default = %q, want signed_cloud", got)
	}
	ch.TrustType = TrustLocalModel
	if got := ch.TrustTypeOrDefault(); got != TrustLocalModel {
		t.Fatalf("explicit trust type = %q, want local_model", got)
	}
}
