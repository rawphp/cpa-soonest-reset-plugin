// Package plugin answers the gateway's plugin methods: register, reconfigure,
// quiesce, scheduler.pick and usage.handle.
package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rawphp/cpa-soonest-reset-plugin/internal/pick"
	"github.com/rawphp/cpa-soonest-reset-plugin/internal/quota"
	"github.com/rawphp/cpa-soonest-reset-plugin/internal/retest"
	"github.com/rawphp/cpa-soonest-reset-plugin/internal/state"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	name    = "soonest-reset"
	version = "0.1.0"
	// The host refuses a plugin whose metadata has no author or repository.
	author     = "rawphp"
	repository = "https://github.com/rawphp/cpa-soonest-reset-plugin"

	defaultRetestMinutes = 60
)

// Plugin holds the account state and the retest schedule. It is safe for concurrent use.
type Plugin struct {
	host      Host
	now       func() time.Time
	tickEvery time.Duration
	store     *state.Store
	schedule  *retest.Schedule
	cfg       atomic.Pointer[config]

	tickerMu sync.Mutex
	stop     chan struct{}
	done     chan struct{}
}

type config struct {
	RetestMinutes int
	LogPicks      bool
}

// New returns a plugin that calls back into host, reads time from now, and
// checks every tickEvery whether a retest is due once it is registered.
func New(host Host, now func() time.Time, tickEvery time.Duration) *Plugin {
	p := &Plugin{
		host:      host,
		now:       now,
		tickEvery: tickEvery,
		store:     state.New(),
		schedule:  retest.NewSchedule(defaultRetestMinutes * time.Minute),
	}
	p.cfg.Store(&config{RetestMinutes: defaultRetestMinutes, LogPicks: true})
	return p
}

// Handle answers one plugin method with a JSON envelope. It never panics.
func (p *Plugin) Handle(method string, request []byte) (response []byte) {
	defer func() {
		if r := recover(); r != nil {
			if method == pluginabi.MethodSchedulerPick {
				response = unhandled()
				return
			}
			response = errorEnvelope("plugin_panic", fmt.Sprintf("%s: %v", method, r))
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.configure(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.stopTicker()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return p.pick(request)
	case pluginabi.MethodUsageHandle:
		return p.usage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
}

// Tick runs a retest pass when one is due. It never panics.
func (p *Plugin) Tick() {
	defer func() { _ = recover() }()
	if !p.schedule.Due(p.now()) {
		return
	}
	result := retest.Pass(p.host, p.store)
	// The gateway's log formatter prints only fields it knows, so the facts go in the message.
	message := fmt.Sprintf("%s: retest reset=%d probe=[%s]", name, len(result.Reset), strings.Join(result.Marked, " "))
	level := "info"
	if len(result.Errors) > 0 {
		level = "warn"
		errs := make([]string, 0, len(result.Errors))
		for _, err := range result.Errors {
			errs = append(errs, err.Error())
		}
		message += " errors=" + strings.Join(errs, "; ")
	}
	_ = p.host.Log(level, message, nil)
}

// Shutdown stops the retest ticker. It is safe to call more than once.
func (p *Plugin) Shutdown() {
	p.stopTicker()
}

// lifecycleRequest matches rpcLifecycleRequest in internal/pluginhost/rpc_schema.go.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type yamlConfig struct {
	RetestMinutes *int  `yaml:"retest_minutes"`
	LogPicks      *bool `yaml:"log_picks"`
}

func (p *Plugin) configure(request []byte) []byte {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode lifecycle request: "+err.Error())
		}
	}
	var raw yamlConfig
	if err := yaml.Unmarshal(req.ConfigYAML, &raw); err != nil {
		return errorEnvelope("invalid_config", "decode plugin config: "+err.Error())
	}
	cfg := config{RetestMinutes: defaultRetestMinutes, LogPicks: true}
	if raw.RetestMinutes != nil {
		cfg.RetestMinutes = *raw.RetestMinutes
	}
	if raw.LogPicks != nil {
		cfg.LogPicks = *raw.LogPicks
	}
	p.cfg.Store(&cfg)
	p.schedule.SetInterval(time.Duration(cfg.RetestMinutes) * time.Minute)
	p.startTicker()
	return okEnvelope(registration())
}

// registrationResult and capabilities match rpcRegistration and rpcCapabilities
// in internal/pluginhost/rpc_schema.go.
type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	UsagePlugin               bool `json:"usage_plugin"`
}

func registration() registrationResult {
	return registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             name,
			Version:          version,
			Author:           author,
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "retest_minutes",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     defaultRetestMinutes,
					Description: "Minutes between cooldown resets of every Claude account. 0 turns the retest off.",
				},
				{
					Name:        "log_picks",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "Log each pick: the account, the rule that chose it and its weekly reset.",
				},
			},
		},
		Capabilities: capabilities{Scheduler: true, SchedulerAcrossPriorities: true, UsagePlugin: true},
	}
}

func (p *Plugin) pick(request []byte) []byte {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return unhandled()
	}
	candidates := make(map[string]pluginapi.SchedulerAuthCandidate, len(req.Candidates))
	ids := make([]string, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if pick.IsClaude(c.Provider) && c.ID != "" {
			candidates[c.ID] = c
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return unhandled()
	}
	var choice pick.Choice
	chosen := p.store.Pick(ids, p.now(), func(views map[string]state.View) string {
		offered := make([]pick.Candidate, 0, len(ids))
		for _, id := range ids {
			c := candidates[id]
			offered = append(offered, pick.Candidate{
				ID:       id,
				Provider: c.Provider,
				Priority: c.Priority,
				Reset7d:  views[id].Reset7d,
				Due:      views[id].Due,
			})
		}
		choice, _ = pick.Choose(offered)
		return choice.ID
	})
	if p.cfg.Load().LogPicks {
		reset := "none"
		if !choice.Reset7d.IsZero() {
			reset = choice.Reset7d.UTC().Format(time.RFC3339)
		}
		_ = p.host.Log("info", fmt.Sprintf("%s: pick account=%s rule=%s reset_7d=%s model=%s",
			name, chosen, choice.Rule, reset, req.Model), nil)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: chosen, Handled: true})
}

func (p *Plugin) usage(request []byte) []byte {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(request, &rec); err != nil {
		return errorEnvelope("invalid_request", "decode usage record: "+err.Error())
	}
	if pick.IsClaude(rec.Provider) && rec.AuthID != "" {
		reset, _ := quota.WeeklyReset(rec.ResponseHeaders, p.now())
		rateLimited := rec.Failed && rec.Failure.StatusCode == 429
		p.store.Observe(rec.AuthID, reset, rateLimited)
	}
	return okEnvelope(struct{}{})
}

func (p *Plugin) startTicker() {
	p.tickerMu.Lock()
	defer p.tickerMu.Unlock()
	if p.stop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	p.stop, p.done = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(p.tickEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.Tick()
			}
		}
	}()
}

func (p *Plugin) stopTicker() {
	p.tickerMu.Lock()
	defer p.tickerMu.Unlock()
	if p.stop == nil {
		return
	}
	close(p.stop)
	<-p.done
	p.stop, p.done = nil, nil
}

func unhandled() []byte {
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

// okEnvelope wraps a result. Every result type here marshals without error.
func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	env, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return env
}

func errorEnvelope(code, message string) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, message)
	return raw
}
