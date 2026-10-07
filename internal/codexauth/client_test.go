package codexauth

import (
	"encoding/json"
	"testing"
)

func TestDeviceCodeAcceptsStringIntervals(t *testing.T) {
	var code DeviceCode
	if err := json.Unmarshal([]byte(`{"device_auth_id":"device","user_code":"ABCD-EFGH","interval":"5","expires_in":"900"}`), &code); err != nil {
		t.Fatalf("unmarshal device code: %v", err)
	}
	if code.Interval != 5 || code.ExpiresIn != 900 {
		t.Fatalf("device code timing = interval %d expires %d, want 5 and 900", code.Interval, code.ExpiresIn)
	}
}

func TestDeviceCodeAcceptsNumericIntervals(t *testing.T) {
	var code DeviceCode
	if err := json.Unmarshal([]byte(`{"device_auth_id":"device","user_code":"ABCD-EFGH","interval":5,"expires_in":900}`), &code); err != nil {
		t.Fatalf("unmarshal device code: %v", err)
	}
	if code.Interval != 5 || code.ExpiresIn != 900 {
		t.Fatalf("device code timing = interval %d expires %d, want 5 and 900", code.Interval, code.ExpiresIn)
	}
}

func TestDeviceCodeAcceptsEmptyExpiry(t *testing.T) {
	var code DeviceCode
	if err := json.Unmarshal([]byte(`{"device_auth_id":"device","user_code":"ABCD-EFGH","interval":"5","expires_in":""}`), &code); err != nil {
		t.Fatalf("unmarshal device code: %v", err)
	}
	if code.Interval != 5 || code.ExpiresIn != 0 {
		t.Fatalf("device code timing = interval %d expires %d, want 5 and 0", code.Interval, code.ExpiresIn)
	}
}
