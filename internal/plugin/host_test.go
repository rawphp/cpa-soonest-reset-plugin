package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rawphp/cpa-soonest-reset-plugin/internal/retest"
)

// The files in testdata are json.Marshal output of the gateway's own v8.0.51
// types (sdk/pluginapi and internal/pluginhost), the way the host encodes them.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func okEnvelopeFor(t *testing.T, result []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type recordedCall struct {
	method  string
	payload string
}

func caller(calls *[]recordedCall, response []byte, err error) Caller {
	return func(method string, payload []byte) ([]byte, error) {
		*calls = append(*calls, recordedCall{method, string(payload)})
		return response, err
	}
}

func TestListAuthsCallsHostAuthListAndKeepsIDIndexAndProvider(t *testing.T) {
	var calls []recordedCall
	host := NewHost(caller(&calls, okEnvelopeFor(t, readFixture(t, "auth_list_result.json")), nil))

	auths, err := host.ListAuths()

	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].method != "host.auth.list" || calls[0].payload != "{}" {
		t.Fatalf("calls = %+v; want one host.auth.list with {}", calls)
	}
	want := []retest.Auth{
		{ID: "claude-acct-a@example.test.json", AuthIndex: "1", Provider: "claude"},
		{ID: "claude-acct-b@example.test.json", AuthIndex: "2", Provider: "claude"},
		{ID: "codex-acct-c@example.test.json", AuthIndex: "3", Provider: "codex"},
	}
	if len(auths) != len(want) {
		t.Fatalf("auths = %+v; want %+v", auths, want)
	}
	for i := range want {
		if auths[i] != want[i] {
			t.Fatalf("auths[%d] = %+v; want %+v", i, auths[i], want[i])
		}
	}
}

func TestResetCooldownSendsTheAuthIndex(t *testing.T) {
	var calls []recordedCall
	host := NewHost(caller(&calls, okEnvelopeFor(t, readFixture(t, "reset_cooldown_result.json")), nil))

	if err := host.ResetCooldown("2"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].method != "host.routing.reset_cooldown" || calls[0].payload != `{"auth_index":"2"}` {
		t.Fatalf("calls = %+v; want one host.routing.reset_cooldown with the auth index", calls)
	}
}

func TestLogSendsLevelMessageAndFields(t *testing.T) {
	var calls []recordedCall
	host := NewHost(caller(&calls, []byte(`{"ok":true,"result":{}}`), nil))

	if err := host.Log("info", "soonest-reset: pick", map[string]any{"account": "claude-acct-a@example.test.json"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].method != "host.log" {
		t.Fatalf("calls = %+v; want one host.log", calls)
	}
	var sent struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(calls[0].payload), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Level != "info" || sent.Message != "soonest-reset: pick" || sent.Fields["account"] != "claude-acct-a@example.test.json" {
		t.Fatalf("host.log payload = %s", calls[0].payload)
	}
}

func TestHostErrorsSurface(t *testing.T) {
	cases := map[string]Caller{
		"error envelope": func(string, []byte) ([]byte, error) {
			return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"auth not found for auth_index 9"}}`), nil
		},
		"error envelope without detail": func(string, []byte) ([]byte, error) {
			return []byte(`{"ok":false}`), nil
		},
		"caller error": func(string, []byte) ([]byte, error) {
			return nil, errors.New("host callback returned no response")
		},
		"not JSON": func(string, []byte) ([]byte, error) {
			return []byte(`<html>`), nil
		},
	}
	for name, call := range cases {
		host := NewHost(call)
		if err := host.ResetCooldown("9"); err == nil {
			t.Errorf("%s: ResetCooldown error = nil", name)
		}
		if _, err := host.ListAuths(); err == nil {
			t.Errorf("%s: ListAuths error = nil", name)
		}
		if err := host.Log("info", "x", nil); err == nil {
			t.Errorf("%s: Log error = nil", name)
		}
	}
}

func TestHostErrorNamesTheMethodAndMessage(t *testing.T) {
	host := NewHost(func(string, []byte) ([]byte, error) {
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"auth not found for auth_index 9"}}`), nil
	})

	err := host.ResetCooldown("9")

	if err == nil || !strings.Contains(err.Error(), "host.routing.reset_cooldown") || !strings.Contains(err.Error(), "auth not found for auth_index 9") {
		t.Fatalf("error = %v; want the method and the host message", err)
	}
}

func TestListAuthsRejectsAMalformedResult(t *testing.T) {
	host := NewHost(func(string, []byte) ([]byte, error) {
		return []byte(`{"ok":true,"result":{"files":"nope"}}`), nil
	})

	if _, err := host.ListAuths(); err == nil {
		t.Fatal("ListAuths error = nil for a malformed result")
	}
}

func TestLogRejectsFieldsThatCannotBeEncoded(t *testing.T) {
	var calls []recordedCall
	host := NewHost(caller(&calls, []byte(`{"ok":true,"result":{}}`), nil))

	if err := host.Log("info", "x", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("Log error = nil for fields that cannot be encoded")
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v; want none", calls)
	}
}
