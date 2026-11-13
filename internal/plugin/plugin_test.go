package plugin

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rawphp/cpa-soonest-reset-plugin/internal/retest"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var start = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

const (
	acctA = "claude-acct-a@example.test.json"
	acctB = "claude-acct-b@example.test.json"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type logLine struct {
	level   string
	message string
	fields  map[string]any
}

type fakeHost struct {
	mu       sync.Mutex
	auths    []retest.Auth
	resets   []string
	lists    int
	logs     []logLine
	panicOn  string
	resetErr error
	listed   chan struct{}
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		auths: []retest.Auth{
			{ID: acctA, AuthIndex: "1", Provider: "claude"},
			{ID: acctB, AuthIndex: "2", Provider: "claude"},
			{ID: "codex-acct-c@example.test.json", AuthIndex: "3", Provider: "codex"},
		},
		listed: make(chan struct{}, 16),
	}
}

func (h *fakeHost) ListAuths() ([]retest.Auth, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.panicOn == "list" {
		panic("host exploded in host.auth.list")
	}
	h.lists++
	select {
	case h.listed <- struct{}{}:
	default:
	}
	return h.auths, nil
}

func (h *fakeHost) ResetCooldown(authIndex string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resets = append(h.resets, authIndex)
	return h.resetErr
}

func (h *fakeHost) Log(level, message string, fields map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.panicOn == "log" {
		panic("host exploded in host.log")
	}
	h.logs = append(h.logs, logLine{level, message, fields})
	return nil
}

func (h *fakeHost) listCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lists
}

// logsWith returns the log lines whose message starts with prefix.
func (h *fakeHost) logsWith(prefix string) []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logLine
	for _, l := range h.logs {
		if strings.HasPrefix(l.message, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// newPlugin returns a plugin whose ticker never fires during a test.
func newPlugin(t *testing.T) (*Plugin, *fakeHost, *clock) {
	t.Helper()
	host := newFakeHost()
	c := &clock{t: start}
	p := New(host, c.now, time.Hour)
	t.Cleanup(p.Shutdown)
	return p, host, c
}

func lifecycle(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte(configYAML), "schema_version": pluginabi.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("response %q is not an envelope: %v", raw, err)
	}
	return env
}

func mustOK(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return env.Result
}

func mustError(t *testing.T, raw []byte, code string) {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != code {
		t.Fatalf("response %s; want an error envelope with code %q", raw, code)
	}
}

func register(t *testing.T, p *Plugin, configYAML string) {
	t.Helper()
	mustOK(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, configYAML)))
}

func pickResponse(t *testing.T, raw []byte) pluginapi.SchedulerPickResponse {
	t.Helper()
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(mustOK(t, raw), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func pickedID(t *testing.T, p *Plugin, request []byte) string {
	t.Helper()
	resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, request))
	if !resp.Handled {
		t.Fatalf("pick unhandled; want a pick")
	}
	return resp.AuthID
}

func pickRequest(t *testing.T, candidates ...pluginapi.SchedulerAuthCandidate) []byte {
	t.Helper()
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(readFixture(t, "pick_request.json"), &req); err != nil {
		t.Fatal(err)
	}
	if candidates != nil {
		req.Candidates = candidates
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func usage(t *testing.T, p *Plugin, authID string, reset time.Time, status int) {
	t.Helper()
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(readFixture(t, "usage_record.json"), &rec); err != nil {
		t.Fatal(err)
	}
	rec.AuthID = authID
	rec.ResponseHeaders = map[string][]string{}
	if !reset.IsZero() {
		rec.ResponseHeaders["Anthropic-Ratelimit-Unified-7d-Reset"] = []string{reset.Format(time.RFC3339)}
	}
	if status != 0 {
		rec.Failed = true
		rec.Failure.StatusCode = status
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, p.Handle(pluginabi.MethodUsageHandle, raw))
}

var (
	wednesday = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	friday    = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
)

// hostRegistration mirrors rpcRegistration and rpcCapabilities in
// internal/pluginhost/rpc_schema.go, the shape the host decodes.
type hostRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]any     `json:"capabilities"`
}

func decodeRegistration(t *testing.T, raw []byte) hostRegistration {
	t.Helper()
	var reg hostRegistration
	if err := json.Unmarshal(mustOK(t, raw), &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterDeclaresSchedulerAcrossPrioritiesAndUsage(t *testing.T) {
	p, _, _ := newPlugin(t)

	reg := decodeRegistration(t, p.Handle(pluginabi.MethodPluginRegister, readFixture(t, "register_request.json")))

	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Errorf("schema_version = %d; want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	want := map[string]any{"scheduler": true, "scheduler_across_priorities": true, "usage_plugin": true}
	if len(reg.Capabilities) != len(want) {
		t.Errorf("capabilities = %v; want exactly %v", reg.Capabilities, want)
	}
	for key, value := range want {
		if reg.Capabilities[key] != value {
			t.Errorf("capabilities[%q] = %v; want %v", key, reg.Capabilities[key], value)
		}
	}
	// The host refuses a plugin whose metadata lacks any of these.
	m := reg.Metadata
	if m.Name != "soonest-reset" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" {
		t.Errorf("metadata = %+v; want name soonest-reset and version, author, repository set", m)
	}
}

func TestRegisterDescribesTheConfigFields(t *testing.T) {
	p, _, _ := newPlugin(t)

	reg := decodeRegistration(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, "")))

	var names []string
	for _, f := range reg.Metadata.ConfigFields {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"retest_minutes", "log_picks"}) {
		t.Fatalf("config fields %v; want retest_minutes, log_picks", names)
	}
}

func TestReconfigureReturnsTheRegistrationAndKeepsState(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "retest_minutes: 60\n")
	usage(t, p, acctA, friday, 0)
	usage(t, p, acctB, wednesday, 0)

	reg := decodeRegistration(t, p.Handle(pluginabi.MethodPluginReconfigure, lifecycle(t, "retest_minutes: 30\nlog_picks: false\n")))

	if reg.Capabilities["scheduler"] != true || reg.Capabilities["usage_plugin"] != true {
		t.Fatalf("reconfigure capabilities = %v", reg.Capabilities)
	}
	if got := pickedID(t, p, pickRequest(t)); got != acctB {
		t.Fatalf("picked %q after reconfigure; want %q from the kept state", got, acctB)
	}
	if n := len(host.logsWith("soonest-reset: pick")); n != 0 {
		t.Fatalf("%d pick logs after log_picks was turned off", n)
	}
}

func TestUsageThenPickGivesTheSoonestResetAccount(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")
	// The fixture is acct-b with a 7d reset of 2026-11-18, as the host sends it.
	mustOK(t, p.Handle(pluginabi.MethodUsageHandle, readFixture(t, "usage_record.json")))
	usage(t, p, acctA, friday, 0)

	resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, readFixture(t, "pick_request.json")))

	if !resp.Handled || resp.AuthID != acctB || resp.Reject || resp.DelegateBuiltin != "" {
		t.Fatalf("pick = %+v; want handled %q", resp, acctB)
	}
}

func TestPickLogsTheAccountRuleAndReset(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "")
	usage(t, p, acctA, friday, 0)
	usage(t, p, acctB, wednesday, 0)

	pickedID(t, p, pickRequest(t))

	logs := host.logsWith("soonest-reset: pick")
	if len(logs) != 1 {
		t.Fatalf("pick logs = %+v; want one", logs)
	}
	// The gateway's log formatter prints only fields it knows, so the facts are in the message.
	want := "soonest-reset: pick account=" + acctB + " rule=soonest-reset reset_7d=2026-11-18T00:00:00Z model=claude-opus-4-5"
	if logs[0].level != "info" || logs[0].message != want {
		t.Fatalf("pick log = %+v; want info %q", logs[0], want)
	}
}

func TestPickLogSaysNoneForAnUnknownReset(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "")

	pickedID(t, p, pickRequest(t))

	logs := host.logsWith("soonest-reset: pick")
	if len(logs) != 1 || !strings.Contains(logs[0].message, " rule=probe reset_7d=none ") {
		t.Fatalf("pick logs = %+v; want one probe with reset none", logs)
	}
}

func TestUnobservedAccountsAreProbedOnceEach(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")
	req := pickRequest(t,
		pluginapi.SchedulerAuthCandidate{ID: acctA, Provider: "claude", Priority: 0},
		pluginapi.SchedulerAuthCandidate{ID: acctB, Provider: "claude", Priority: 5},
	)

	first := pickedID(t, p, req)
	second := pickedID(t, p, req)
	usage(t, p, acctB, friday, 0)
	usage(t, p, acctA, wednesday, 0)
	third := pickedID(t, p, req)

	if first != acctB || second != acctA || third != acctA {
		t.Fatalf("picks = %q, %q, %q; want the higher priority probed first, then the other, then the soonest reset", first, second, third)
	}
}

func TestPickWithNoClaudeCandidatesIsUnhandled(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")

	for name, req := range map[string][]byte{
		"other providers": pickRequest(t, pluginapi.SchedulerAuthCandidate{ID: "codex-acct-c@example.test.json", Provider: "codex"}),
		"no candidates":   []byte(`{"Provider":"claude","Model":"claude-opus-4-5","Candidates":[]}`),
	} {
		if resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, req)); resp.Handled || resp.AuthID != "" {
			t.Errorf("%s: pick = %+v; want unhandled", name, resp)
		}
	}
}

func TestMixedProvidersPickOnlyClaude(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")
	usage(t, p, acctA, friday, 0)

	got := pickedID(t, p, pickRequest(t,
		pluginapi.SchedulerAuthCandidate{ID: "codex-acct-c@example.test.json", Provider: "codex", Priority: 9},
		pluginapi.SchedulerAuthCandidate{ID: acctA, Provider: "claude"},
	))

	if got != acctA {
		t.Fatalf("picked %q; want %q", got, acctA)
	}
}

func TestUsageFromOtherProvidersIsIgnored(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")
	usage(t, p, acctA, friday, 0)
	usage(t, p, acctB, wednesday, 0)
	codex := []byte(`{"Provider":"codex","AuthID":"claude-acct-a@example.test.json","ResponseHeaders":{"Anthropic-Ratelimit-Unified-7d-Reset":["2026-11-15T00:00:00Z"]}}`)

	mustOK(t, p.Handle(pluginabi.MethodUsageHandle, codex))

	if got := pickedID(t, p, pickRequest(t)); got != acctB {
		t.Fatalf("picked %q; a codex record changed a Claude account's reset", got)
	}
}

func TestUsageWithoutAnAuthIDIsIgnored(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")

	mustOK(t, p.Handle(pluginabi.MethodUsageHandle, []byte(`{"Provider":"claude","AuthID":""}`)))
}

func TestARecordWithoutTheHeaderStillCountsAsObserved(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "")
	req := pickRequest(t, pluginapi.SchedulerAuthCandidate{ID: acctA, Provider: "claude"})
	pickedID(t, p, req) // probe
	usage(t, p, acctA, time.Time{}, 0)

	p.Handle(pluginabi.MethodSchedulerPick, req)
	p.Handle(pluginabi.MethodSchedulerPick, req)

	logs := host.logsWith("soonest-reset: pick")
	if len(logs) != 3 {
		t.Fatalf("pick logs = %+v; want three", logs)
	}
	for _, l := range logs[1:] {
		if !strings.Contains(l.message, " rule=soonest-reset ") {
			t.Fatalf("pick after a header-less answer logged %q; want rule soonest-reset, not another probe", l.message)
		}
	}
}

func TestRetestRunsOnlyAfterTheIntervalAndProbesThe429dAccount(t *testing.T) {
	p, host, c := newPlugin(t)
	register(t, p, "retest_minutes: 60\n")
	usage(t, p, acctA, wednesday, 0)
	usage(t, p, acctB, friday, 429)

	c.advance(59 * time.Minute)
	p.Tick()
	if host.listCount() != 0 {
		t.Fatal("retest ran before the interval passed")
	}

	c.advance(time.Minute)
	p.Tick()
	if !slices.Equal(host.resets, []string{"1", "2"}) {
		t.Fatalf("reset auth indexes %v; want both Claude accounts", host.resets)
	}
	if got := pickedID(t, p, pickRequest(t)); got != acctB {
		t.Fatalf("picked %q after the retest; want the 429'd account %q probed", got, acctB)
	}
	if got := pickedID(t, p, pickRequest(t)); got != acctA {
		t.Fatalf("picked %q on the next request; want %q by soonest reset", got, acctA)
	}
	logs := host.logsWith("soonest-reset: retest")
	if len(logs) != 1 || logs[0].level != "info" || logs[0].message != "soonest-reset: retest reset=2 probe=["+acctB+"]" {
		t.Fatalf("retest logs = %+v", logs)
	}
}

func TestRetestMinutesZeroNeverRuns(t *testing.T) {
	p, host, c := newPlugin(t)
	register(t, p, "retest_minutes: 0\n")

	c.advance(1000 * time.Hour)
	p.Tick()

	if host.listCount() != 0 {
		t.Fatal("retest ran with retest_minutes 0")
	}
}

func TestDefaultsAreSixtyMinutesAndLoggedPicks(t *testing.T) {
	p, host, c := newPlugin(t)
	register(t, p, "enabled: true\npriority: 1\n")

	c.advance(59 * time.Minute)
	p.Tick()
	if host.listCount() != 0 {
		t.Fatal("default retest ran before 60 minutes")
	}
	c.advance(time.Minute)
	p.Tick()
	if host.listCount() != 1 {
		t.Fatal("default retest did not run at 60 minutes")
	}
	pickedID(t, p, pickRequest(t))
	if len(host.logsWith("soonest-reset: pick")) != 1 {
		t.Fatal("picks not logged by default")
	}
}

func TestTheTickerRunsTheRetestAndStopsAtShutdown(t *testing.T) {
	host := newFakeHost()
	c := &clock{t: start}
	p := New(host, c.now, time.Millisecond)
	register(t, p, "retest_minutes: 1\n")

	c.advance(time.Minute)
	select {
	case <-host.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker did not run the retest")
	}

	p.Shutdown()
	before := host.listCount()
	c.advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if host.listCount() != before {
		t.Fatal("the ticker ran after shutdown")
	}
	p.Shutdown() // a second shutdown is harmless
}

func TestQuiesceStopsTheTickerAndReconfigureRestartsIt(t *testing.T) {
	host := newFakeHost()
	c := &clock{t: start}
	p := New(host, c.now, time.Millisecond)
	t.Cleanup(p.Shutdown)
	register(t, p, "retest_minutes: 1\n")

	mustOK(t, p.Handle(pluginabi.MethodPluginQuiesce, []byte(`{}`)))
	c.advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if host.listCount() != 0 {
		t.Fatal("the ticker ran after quiesce")
	}

	mustOK(t, p.Handle(pluginabi.MethodPluginReconfigure, lifecycle(t, "retest_minutes: 1\n")))
	select {
	case <-host.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker did not restart after reconfigure")
	}
}

func TestMalformedRequestsGetAnEnvelope(t *testing.T) {
	p, _, _ := newPlugin(t)
	register(t, p, "")

	mustError(t, p.Handle(pluginabi.MethodPluginRegister, []byte(`{not json`)), "invalid_request")
	mustError(t, p.Handle(pluginabi.MethodPluginRegister, lifecycle(t, "retest_minutes: [1, 2]\n")), "invalid_config")
	mustError(t, p.Handle(pluginabi.MethodUsageHandle, []byte(`{not json`)), "invalid_request")
	mustError(t, p.Handle("executor.execute", []byte(`{}`)), "unknown_method")
	// A scheduler error would fail the request, so bad pick input is left to the built-in scheduler.
	if resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, []byte(`{not json`))); resp.Handled {
		t.Fatalf("malformed pick = %+v; want unhandled", resp)
	}
	if resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, nil)); resp.Handled {
		t.Fatalf("empty pick = %+v; want unhandled", resp)
	}
}

func TestAnInvalidReconfigureKeepsTheOldConfig(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "log_picks: false\n")

	mustError(t, p.Handle(pluginabi.MethodPluginReconfigure, lifecycle(t, "log_picks: [oops]\n")), "invalid_config")
	pickedID(t, p, pickRequest(t))

	if n := len(host.logsWith("soonest-reset: pick")); n != 0 {
		t.Fatalf("%d pick logs; the failed reconfigure replaced the config", n)
	}
}

func TestAPanickingHostDuringAPickIsContained(t *testing.T) {
	p, host, _ := newPlugin(t)
	register(t, p, "")
	host.panicOn = "log"

	resp := pickResponse(t, p.Handle(pluginabi.MethodSchedulerPick, pickRequest(t)))

	if resp.Handled {
		t.Fatalf("pick = %+v; want unhandled after a panic", resp)
	}
}

func TestAPanickingHostDuringARetestIsContained(t *testing.T) {
	p, host, c := newPlugin(t)
	register(t, p, "retest_minutes: 1\n")
	host.panicOn = "list"
	c.advance(time.Minute)

	p.Tick() // must not panic
}

func TestAPanicInAnyOtherMethodGivesAnErrorEnvelope(t *testing.T) {
	host := newFakeHost()
	exploding := false
	p := New(host, func() time.Time {
		if exploding {
			panic("clock exploded")
		}
		return start
	}, time.Hour)
	t.Cleanup(p.Shutdown)
	register(t, p, "")
	exploding = true

	raw := p.Handle(pluginabi.MethodUsageHandle, readFixture(t, "usage_record.json"))

	mustError(t, raw, "plugin_panic")
	if env := decodeEnvelope(t, raw); !strings.Contains(env.Error.Message, "clock exploded") {
		t.Fatalf("panic envelope = %s; want the panic value", raw)
	}
}

func TestHostLogErrorsDoNotFailAPick(t *testing.T) {
	host := &erroringLogHost{fakeHost: newFakeHost()}
	p := New(host, (&clock{t: start}).now, time.Hour)
	t.Cleanup(p.Shutdown)
	register(t, p, "")

	if got := pickedID(t, p, pickRequest(t)); got != acctA {
		t.Fatalf("picked %q; want %q", got, acctA)
	}
}

type erroringLogHost struct{ *fakeHost }

func (erroringLogHost) Log(string, string, map[string]any) error {
	return errors.New("host plugin callback instance is closed")
}

func TestRetestErrorsAreLoggedAsAWarning(t *testing.T) {
	p, host, c := newPlugin(t)
	register(t, p, "retest_minutes: 1\n")
	host.resetErr = errors.New("auth not found")
	c.advance(time.Minute)

	p.Tick()

	logs := host.logsWith("soonest-reset: retest")
	if len(logs) != 1 || logs[0].level != "warn" || !strings.HasPrefix(logs[0].message, "soonest-reset: retest reset=0 probe=[] errors=reset "+acctA+": auth not found; reset "+acctB+": auth not found") {
		t.Fatalf("retest logs = %+v; want one warning with both errors", logs)
	}
}
