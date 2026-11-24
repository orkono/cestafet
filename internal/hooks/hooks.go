// Package hooks implements the Claude Code hook handlers: they register the
// session, keep it alive and inject pending notices into its context.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/orkono/cestafet/internal/core"
	"github.com/orkono/cestafet/internal/identity"
)

// Input is the subset of the hook payload cestafet reads.
type Input struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Source    string `json:"source"` // SessionStart: startup, resume, clear, compact
	Prompt    string `json:"prompt"` // UserPromptSubmit
	Reason    string `json:"reason"` // SessionEnd: clear, logout, prompt_input_exit, other
	// AgentID is set when the tool call was made by a subagent. Its context is
	// not the main conversation's, so notices are held for the main agent.
	AgentID string `json:"agent_id"`
	// PreToolUse (kept raw so PostToolUse — matcher "*" — never fails when another
	// tool's tool_input has a non-string "command" field or non-object shape).
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type specificOutput struct {
	HookEventName            string `json:"hookEventName"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

type output struct {
	HookSpecificOutput specificOutput `json:"hookSpecificOutput"`
}

// Events maps the CLI event names to Claude Code hook event names.
var Events = map[string]string{
	"session-start": "SessionStart",
	"prompt":        "UserPromptSubmit",
	"post-tool":     "PostToolUse",
	"pre-tool":      "PreToolUse",
	"session-end":   "SessionEnd",
}

// Handle runs one hook event. It reads the payload from in and writes the hook
// response (if any) to out. claude identifies the owning Claude Code process.
func Handle(app *core.App, event string, in io.Reader, out io.Writer, claude identity.Proc) error {
	name, ok := Events[event]
	if !ok {
		return fmt.Errorf("unknown hook event %q", event)
	}
	var input Input
	if err := json.NewDecoder(in).Decode(&input); err != nil {
		return fmt.Errorf("decode hook payload: %w", err)
	}
	if input.SessionID == "" {
		return fmt.Errorf("hook payload has no session_id")
	}

	if name == "PreToolUse" {
		reason, err := guardReason(app, input)
		if err != nil || reason == "" {
			return err
		}
		return json.NewEncoder(out).Encode(output{specificOutput{HookEventName: name,
			PermissionDecision: "ask", PermissionDecisionReason: reason}})
	}

	context, err := run(app, name, input, claude)
	if err != nil || context == "" {
		return err
	}
	return json.NewEncoder(out).Encode(output{specificOutput{HookEventName: name, AdditionalContext: context}})
}

func run(app *core.App, name string, in Input, claude identity.Proc) (string, error) {
	switch name {
	case "SessionEnd":
		if in.Reason == "clear" {
			// /clear: the same claude process starts a new session id right away;
			// registering it retires this one and carries the coordinator role over.
			return "", nil
		}
		return "", app.Store.End(in.SessionID)

	case "SessionStart":
		sess, _, err := app.Register(in.SessionID, in.Cwd, claude)
		if err != nil {
			return "", err
		}
		return join(app.Orientation(sess), pending(app, sess.ID)), nil

	case "UserPromptSubmit":
		// Re-resolve the project on each prompt so a /cestafet:project change or
		// a cwd change is picked up; cheap compared to a model turn.
		sess, isNew, err := app.Register(in.SessionID, in.Cwd, claude)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(in.Prompt) != "" {
			if _, err := app.Store.RecordPrompt(sess.ID, in.Prompt); err != nil {
				return "", err
			}
		}
		orient := ""
		if isNew { // plugin installed mid-session: no SessionStart was seen
			orient = app.Orientation(sess)
		}
		return join(orient, pending(app, sess.ID)), nil

	default: // PostToolUse: must stay cheap, no git calls
		if _, err := app.Store.Get(in.SessionID); err != nil {
			if _, _, err := app.Register(in.SessionID, in.Cwd, claude); err != nil {
				return "", err
			}
		} else if err := app.Store.Touch(in.SessionID); err != nil {
			return "", err
		}
		if in.AgentID != "" {
			return "", nil
		}
		return pending(app, in.SessionID), nil
	}
}

func pending(app *core.App, sessionID string) string {
	notices, left, err := app.Store.TakePendingWithin(sessionID, core.MaxInjectBytes)
	if err != nil {
		return ""
	}
	out := core.RenderNotices(notices)
	if left > 0 {
		out += fmt.Sprintf("\n\n(cestafet: %d more notice(s) pending; they arrive with your next step.)", left)
	}
	return out
}

func join(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}
