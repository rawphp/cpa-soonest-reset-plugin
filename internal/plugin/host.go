package plugin

import (
	"encoding/json"
	"fmt"

	"cpa-soonest-reset/internal/retest"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Caller sends one host callback and returns the raw response envelope.
// main.go supplies the cgo implementation.
type Caller func(method string, payload []byte) ([]byte, error)

// Host is the part of the gateway the plugin calls back into.
type Host interface {
	retest.Host
	Log(level, message string, fields map[string]any) error
}

// NewHost returns a Host that speaks the gateway's callback protocol through call.
func NewHost(call Caller) Host {
	return callbackHost{call: call}
}

type callbackHost struct {
	call Caller
}

// logRequest matches rpcHostLogRequest in internal/pluginhost/host_callbacks.go.
type logRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// authListResult matches rpcHostAuthListResponse in internal/pluginhost/auth_callbacks.go.
type authListResult struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

func (h callbackHost) ListAuths() ([]retest.Auth, error) {
	raw, err := h.invoke(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var result authListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode %s result: %w", pluginabi.MethodHostAuthList, err)
	}
	auths := make([]retest.Auth, 0, len(result.Files))
	for _, f := range result.Files {
		auths = append(auths, retest.Auth{ID: f.ID, AuthIndex: f.AuthIndex, Provider: f.Provider})
	}
	return auths, nil
}

func (h callbackHost) ResetCooldown(authIndex string) error {
	_, err := h.invoke(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex})
	return err
}

func (h callbackHost) Log(level, message string, fields map[string]any) error {
	_, err := h.invoke(pluginabi.MethodHostLog, logRequest{Level: level, Message: message, Fields: fields})
	return err
}

func (h callbackHost) invoke(method string, request any) (json.RawMessage, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	raw, err := h.call(method, payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s failed", method)
	}
	return env.Result, nil
}
