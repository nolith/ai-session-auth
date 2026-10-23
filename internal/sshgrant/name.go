package sshgrant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// toolName is what a tool, the first part of an agent's name, may look like.
var toolName = regexp.MustCompile(`^[a-z0-9-]+$`)

// herdrTimeout bounds the question to herdr: a session asks it before the
// harness starts.
const herdrTimeout = 5 * time.Second

// Name is the SSH name of the agent tool runs as: <tool>-<workspace>, where
// workspace is the label of the herdr workspace the agent runs in. When
// AGENT_SSH_NAME already names an agent of tool, as inside a session, Name
// is that, and needs no herdr, which a sandbox may keep out of reach.
// Without tool, Name is AGENT_SSH_NAME.
func Name(ctx context.Context, tool string) (string, error) {
	known := os.Getenv("AGENT_SSH_NAME")
	var name string
	switch {
	case tool != "" && strings.HasPrefix(known, tool+"-"):
		name = Slug(known)
	case tool != "":
		if !toolName.MatchString(tool) {
			return "", errors.New("bad tool name: " + tool)
		}
		label, err := workspaceLabel(ctx)
		if err != nil {
			return "", err
		}
		if label = Slug(label); label == "" {
			return "", errors.New("the herdr workspace has no usable label")
		}
		name = tool + "-" + label
	case known != "":
		name = Slug(known)
	default:
		return "", errors.New("no agent: give a <tool>, or run inside a geas session")
	}
	if name == "" {
		return "", errors.New("empty agent name")
	}
	return name, nil
}

// workspaceLabel asks herdr for the label of the workspace HERDR_WORKSPACE_ID
// names.
func workspaceLabel(ctx context.Context) (string, error) {
	id := os.Getenv("HERDR_WORKSPACE_ID")
	if id == "" {
		return "", errors.New("not in a herdr workspace, and AGENT_SSH_NAME names no agent of this tool")
	}
	herdr, err := exec.LookPath("herdr")
	if err != nil {
		return "", errors.New("herdr not found, and AGENT_SSH_NAME names no agent of this tool")
	}
	ctx, cancel := context.WithTimeout(ctx, herdrTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, herdr, "workspace", "get", id).Output()
	if err != nil {
		return "", errors.New("cannot ask herdr for the workspace's label")
	}
	var answer struct {
		Result struct {
			Workspace struct {
				Label string `json:"label"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return "", errors.New("cannot read herdr's answer about the workspace")
	}
	return answer.Result.Workspace.Label, nil
}
