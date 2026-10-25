// Package adminpipe is the local control channel between the burrow CLI and
// the running service.
//
// This is the one listener in Burrow that is not a tsnet listener. It is a
// Windows named pipe, not a socket, so it is reachable only from this machine
// and only by the accounts in its ACL — which is what lets `burrow unlock`
// clear lockdown from the console while no network client can
// (docs/03-SECURITY.md §4).
package adminpipe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Name is the pipe path.
const Name = `\\.\pipe\burrow-admin`

// Command is a request from the CLI.
type Command struct {
	Op string `json:"op"`
	// Args carries per-operation parameters.
	Args map[string]string `json:"args,omitempty"`
}

// Operations the service accepts.
const (
	OpStatus       = "status"
	OpPairOpen     = "pair.open"
	OpPairClose    = "pair.close"
	OpUnlock       = "unlock"
	OpDevices      = "devices.list"
	OpDeviceRevoke = "devices.revoke"
	OpSelftest     = "selftest"
	OpKillAll      = "sessions.kill-all"
)

// Response is the service's reply.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// StatusData is the payload of OpStatus.
type StatusData struct {
	Version   string `json:"version"`
	FQDN      string `json:"fqdn"`
	Lockdown  bool   `json:"lockdown"`
	Sessions  int    `json:"sessions"`
	Devices   int    `json:"devices"`
	Uptime    string `json:"uptime"`
	KeepAwake bool   `json:"keepAwake"`
}

// PairData is the payload of OpPairOpen.
type PairData struct {
	Code    string    `json:"code"`
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

// DeviceRow is one entry of OpDevices.
type DeviceRow struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	NodeID   string `json:"nodeId"`
	LastSeen string `json:"lastSeen,omitempty"`
}

// SelftestResult is the payload of OpSelftest.
type SelftestResult struct {
	Checks []Check `json:"checks"`
	Passed bool    `json:"passed"`
}

// Check is one self-test line.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Handler serves commands. Implemented by the agent.
type Handler func(Command) Response

// ErrNotRunning means nothing is listening on the pipe.
var ErrNotRunning = errors.New("adminpipe: the Burrow service does not appear to be running")

// dialTimeout bounds how long the CLI waits for the service.
const dialTimeout = 5 * time.Second

// Call sends one command and returns the response.
func Call(cmd Command) (Response, error) {
	conn, err := dial(dialTimeout)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()

	// A deadline on both halves, so a wedged service cannot hang the CLI.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		return Response{}, fmt.Errorf("adminpipe: send: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("adminpipe: read reply: %w", err)
	}
	return resp, nil
}

// Serve accepts connections and dispatches each command to h.
func Serve(ln net.Listener, h Handler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(60 * time.Second))

			var cmd Command
			if err := json.NewDecoder(c).Decode(&cmd); err != nil {
				_ = json.NewEncoder(c).Encode(Response{OK: false, Error: "malformed command"})
				return
			}
			resp := h(cmd)
			_ = json.NewEncoder(c).Encode(resp)
		}(conn)
	}
}

// OK builds a successful response with an optional payload.
func OK(payload any) Response {
	if payload == nil {
		return Response{OK: true}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Response{OK: false, Error: "could not encode the reply"}
	}
	return Response{OK: true, Data: raw}
}

// Fail builds an error response.
func Fail(format string, a ...any) Response {
	return Response{OK: false, Error: fmt.Sprintf(format, a...)}
}

// Decode unmarshals a response payload.
func Decode[T any](r Response) (T, error) {
	var out T
	if !r.OK {
		return out, errors.New(r.Error)
	}
	if len(r.Data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(r.Data, &out); err != nil {
		return out, fmt.Errorf("adminpipe: decode reply: %w", err)
	}
	return out, nil
}
