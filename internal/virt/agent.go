package virt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GuestExec runs a shell script inside a running guest through the QEMU guest
// agent and returns its exit code and output (stdout + stderr). It waits up
// to timeout for the script to end.
func (v *Virsh) GuestExec(ctx context.Context, name, script string, timeout time.Duration) (int, string, error) {
	req, _ := json.Marshal(map[string]any{"execute": "guest-exec", "arguments": map[string]any{
		"path": "/bin/sh", "arg": []string{"-c", script}, "capture-output": true}})
	out, err := v.virsh(ctx, "qemu-agent-command", name, string(req))
	if err != nil {
		return -1, "", fmt.Errorf("guest agent: %w", err)
	}
	var started struct {
		Return struct {
			PID int `json:"pid"`
		} `json:"return"`
	}
	if err := json.Unmarshal(out, &started); err != nil {
		return -1, "", fmt.Errorf("guest agent answer: %w", err)
	}
	status, _ := json.Marshal(map[string]any{"execute": "guest-exec-status", "arguments": map[string]int{"pid": started.Return.PID}})
	deadline := time.Now().Add(timeout)
	for {
		out, err := v.virsh(ctx, "qemu-agent-command", name, string(status))
		if err != nil {
			return -1, "", fmt.Errorf("guest agent: %w", err)
		}
		done, code, text, err := ParseExecStatus(out)
		if err != nil {
			return -1, "", err
		}
		if done {
			return code, text, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return -1, text, fmt.Errorf("the command in the guest didn't finish within %s", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ParseExecStatus reads a guest-exec-status answer.
func ParseExecStatus(b []byte) (done bool, code int, out string, err error) {
	var st struct {
		Return struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			Signal   int    `json:"signal"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		} `json:"return"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return false, 0, "", fmt.Errorf("guest agent answer: %w", err)
	}
	var sb strings.Builder
	for _, d := range []string{st.Return.OutData, st.Return.ErrData} {
		if d == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(d)
		if err == nil {
			sb.Write(raw)
		}
	}
	code = st.Return.ExitCode
	if st.Return.Signal != 0 && code == 0 {
		code = 128 + st.Return.Signal
	}
	return st.Return.Exited, code, sb.String(), nil
}
