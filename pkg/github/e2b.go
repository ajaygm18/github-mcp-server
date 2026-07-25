package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var ToolsetMetadataE2B = inventory.ToolsetMetadata{
	ID:          "e2b",
	Description: "Official E2B Cloud Desktop Sandbox, VNC Stream, File Manager, and Code Interpreter tools supporting single-shot and persistent multi-command sandbox sessions. Always use e2b_list_sandboxes to audit for active sandboxes.",
	Default:     true,
	Icon:        "tools",
}

// Session & active sandbox memory tracking
type sandboxTracker struct {
	mu                  sync.Mutex
	lastActiveSandboxID string
	lastActiveTime      time.Time
}

var globalSandboxTracker sandboxTracker
var startReaperOnce sync.Once

func getLastActiveSandboxID() string {
	globalSandboxTracker.mu.Lock()
	defer globalSandboxTracker.mu.Unlock()
	return globalSandboxTracker.lastActiveSandboxID
}

func updateLastActiveSandboxID(id string) {
	if id == "" {
		return
	}
	globalSandboxTracker.mu.Lock()
	defer globalSandboxTracker.mu.Unlock()
	globalSandboxTracker.lastActiveSandboxID = id
	globalSandboxTracker.lastActiveTime = time.Now()
}

func clearLastActiveSandboxID(id string) {
	globalSandboxTracker.mu.Lock()
	defer globalSandboxTracker.mu.Unlock()
	if id == "" || globalSandboxTracker.lastActiveSandboxID == id {
		globalSandboxTracker.lastActiveSandboxID = ""
	}
}

// Start automatic server-side idle reaper
func initIdleReaper() {
	startReaperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(1 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				reapIdleSandboxes()
			}
		}()
	})
}

func reapIdleSandboxes() {
	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		return
	}

	idleMinutesStr := os.Getenv("E2B_IDLE_REAPER_MINUTES")
	idleMinutes := 15
	if m, err := strconv.Atoi(idleMinutesStr); err == nil && m > 0 {
		idleMinutes = m
	}

	pyScript := fmt.Sprintf(`
import os, json
from e2b import Sandbox
from datetime import datetime, timezone

max_idle_sec = %d * 60
try:
    pag = Sandbox.list()
    items = pag.next_items()
    now = datetime.now(timezone.utc)
    reaped = []
    for i in items:
        if i.started_at:
            uptime = (now - i.started_at).total_seconds()
            if uptime > max_idle_sec:
                try:
                    sbx = Sandbox.connect(i.sandbox_id)
                    sbx.kill()
                    reaped.append(i.sandbox_id)
                except Exception:
                    pass
    print("REAPED:" + json.dumps(reaped))
except Exception:
    pass
`, idleMinutes)

	out, err := runE2BPythonScript(apiKey, pyScript)
	_, reapedJSON, foundReaped := strings.Cut(out, "REAPED:")
	if err == nil && foundReaped {
		var reaped []string
		_ = json.Unmarshal([]byte(reapedJSON), &reaped)
		for _, id := range reaped {
			slog.Info("[E2B Idle Reaper] Auto-destroyed expired sandbox", "sandbox_id", id)
			clearLastActiveSandboxID(id)
		}
	}
}

func getE2BAPIKey(args map[string]any) string {
	if apiKey, ok := args["api_key"].(string); ok && apiKey != "" {
		return apiKey
	}
	return os.Getenv("E2B_API_KEY")
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

type e2bJSONResponse struct {
	Status         string           `json:"status"`
	SandboxID      string           `json:"sandbox_id"`
	ReusedExisting bool             `json:"reused_existing"`
	ExitCode       int              `json:"exit_code"`
	PID            *int             `json:"pid,omitempty"`
	Stdout         string           `json:"stdout"`
	Stderr         string           `json:"stderr"`
	ExecutionError string           `json:"execution_error,omitempty"`
	Results        []string         `json:"results,omitempty"`
	DurationMS     int              `json:"duration_ms"`
	IsPersistent   bool             `json:"is_persistent"`
	Message        string           `json:"message,omitempty"`
	Error          string           `json:"error,omitempty"`
	Sandboxes      []map[string]any `json:"sandboxes,omitempty"`
}

func parseE2BJSONResponse(output string) (*e2bJSONResponse, error) {
	startIdx := strings.Index(output, "---E2B_JSON_START---")
	endIdx := strings.Index(output, "---E2B_JSON_END---")
	if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
		jsonStr := strings.TrimSpace(output[startIdx+len("---E2B_JSON_START---") : endIdx])
		var resp e2bJSONResponse
		if err := json.Unmarshal([]byte(jsonStr), &resp); err == nil {
			return &resp, nil
		}
	}
	return nil, fmt.Errorf("raw output: %s", output)
}

func runE2BPythonScript(apiKey, pyCode string) (string, error) {
	initIdleReaper()

	// Heroku router enforces a strict 30-second H12 timeout on all HTTP requests.
	// We set a 24-second server-side timeout so we intercept execution BEFORE
	// Heroku cuts the connection and outputs an HTML 503 error page.
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-c", pyCode)
	cmd.Env = append(os.Environ(), fmt.Sprintf("E2B_API_KEY=%s", apiKey))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start python runner: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		partialOut := strings.TrimSpace(stdout.String())
		if partialOut == "" {
			partialOut = strings.TrimSpace(stderr.String())
		}
		return fmt.Sprintf("E2B Command Timed Out (Reached 24s Heroku Safety Limit).\n"+
			"Note: Heroku HTTP router enforces a strict 30s timeout on web requests.\n"+
			"For long builds or compilation, set 'background': true on e2b_run_command, or break commands into smaller steps.\n"+
			"Partial Output Collected:\n%s", partialOut), nil
	case err := <-done:
		if err != nil {
			errStr := stderr.String()
			if errStr == "" {
				errStr = stdout.String()
			}
			// Clean up raw python stack traces for infrastructure errors
			lines := strings.Split(strings.TrimSpace(errStr), "\n")
			cleanMsg := lines[len(lines)-1]
			return "", fmt.Errorf("%s", cleanMsg)
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

// 1. E2BRunCode: Code Interpreter execution
func E2BRunCode(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_code",
			Description: t("TOOL_E2B_RUN_CODE_DESCRIPTION", "Run Python code inside official E2B Code Interpreter cloud sandbox. Set 'keep_alive': true to persist the sandbox. Note: keep_alive bills for wall-clock uptime; call e2b_kill_sandbox when finished."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_RUN_CODE_TITLE", "Run Code in E2B Sandbox"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"code": {
						Type:        "string",
						Description: "The Python code to execute in the E2B sandbox",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. If you created a sandbox earlier in this session, you must pass its ID here. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"code"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			code, err := RequiredParam[string](args, "code")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json, sys, time
from e2b_code_interpreter import Sandbox

code_to_run = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    t0 = time.time()
    execution = sbx.run_code(code_to_run)
    stdout_txt = "".join([str(l) for l in execution.logs.stdout])
    stderr_txt = "".join([str(l) for l in execution.logs.stderr])
    error_txt = ""
    if execution.error:
        error_txt = f"{execution.error.name}: {execution.error.value}\n{execution.error.traceback}"

    dur = int((time.time() - t0) * 1000)
    is_persistent = bool(sbx_id) or keep_alive or reused_existing

    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "stdout": stdout_txt,
        "stderr": stderr_txt,
        "execution_error": error_txt,
        "results": [str(r) for r in execution.results] if execution.results else [],
        "duration_ms": dur,
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(code), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			resJSON, _ := json.MarshalIndent(map[string]any{
				"sandbox_id":      parsed.SandboxID,
				"reused_existing": parsed.ReusedExisting,
				"stdout":          parsed.Stdout,
				"stderr":          parsed.Stderr,
				"execution_error": parsed.ExecutionError,
				"results":         parsed.Results,
				"duration_ms":     parsed.DurationMS,
			}, "", "  ")

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v] [duration: %dms]\n\nSTDOUT:\n%s",
				parsed.SandboxID, parsed.ReusedExisting, parsed.DurationMS, parsed.Stdout)
			if parsed.Stderr != "" {
				outText += "\n\nSTDERR:\n" + parsed.Stderr
			}
			if parsed.ExecutionError != "" {
				outText += "\n\nEXECUTION_ERROR:\n" + parsed.ExecutionError
			}
			outText += "\n\nStructured Result:\n" + string(resJSON)

			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 2. E2BRunCommand: Terminal command execution
func E2BRunCommand(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_command",
			Description: t("TOOL_E2B_RUN_COMMAND_DESCRIPTION", "Run terminal commands inside an E2B cloud sandbox. Set 'keep_alive': true to persist the sandbox session. Non-zero command exit codes return structured output with isError: false. CRITICAL: For long-running builds or compilation (like 'go build', 'npm install', 'docker build'), ALWAYS set 'background': true."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_RUN_COMMAND_TITLE", "Run Command in E2B"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"command": {
						Type:        "string",
						Description: "The terminal command to execute",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. If you created a sandbox earlier in this session, you must pass its ID here. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"background": {
						Type:        "boolean",
						Description: "Set true for long-running builds/compilations to run in background asynchronously without blocking or timing out",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"command"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			command, err := RequiredParam[string](args, "command")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			background, _ := OptionalParam[bool](args, "background")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json, sys, time
from e2b import Sandbox, CommandExitException

cmd_to_run = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
is_background = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    t0 = time.time()
    exit_code = 0
    stdout_txt = ""
    stderr_txt = ""
    pid = None

    if is_background:
        res = sbx.commands.run(cmd_to_run, background=True)
        pid = getattr(res, "pid", None)
        stdout_txt = f"Command successfully launched in background (PID: {pid})"
    else:
        try:
            res = sbx.commands.run(cmd_to_run)
            exit_code = getattr(res, "exit_code", 0)
            stdout_txt = getattr(res, "stdout", "") or ""
            stderr_txt = getattr(res, "stderr", "") or ""
        except CommandExitException as e:
            exit_code = getattr(e, "exit_code", 1)
            stdout_txt = getattr(e, "stdout", "") or ""
            stderr_txt = getattr(e, "stderr", "") or getattr(e, "error", "") or str(e)

    dur = int((time.time() - t0) * 1000)
    is_persistent = bool(sbx_id) or keep_alive or is_background or reused_existing

    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "exit_code": exit_code,
        "stdout": stdout_txt,
        "stderr": stderr_txt,
        "duration_ms": dur,
        "is_persistent": is_persistent
    }
    if pid is not None:
        res_payload["pid"] = pid

    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or is_background or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(command), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), pyBool(background), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			resJSON, _ := json.MarshalIndent(map[string]any{
				"sandbox_id":      parsed.SandboxID,
				"reused_existing": parsed.ReusedExisting,
				"exit_code":       parsed.ExitCode,
				"stdout":          parsed.Stdout,
				"stderr":          parsed.Stderr,
				"duration_ms":     parsed.DurationMS,
			}, "", "  ")

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v] [exit_code: %d] [duration: %dms]\n\nSTDOUT:\n%s",
				parsed.SandboxID, parsed.ReusedExisting, parsed.ExitCode, parsed.DurationMS, parsed.Stdout)
			if parsed.Stderr != "" {
				outText += "\n\nSTDERR:\n" + parsed.Stderr
			}
			outText += "\n\nStructured Result:\n" + string(resJSON)

			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 3. E2BListSandboxes: Enumerate active and paused E2B cloud sandboxes
func E2BListSandboxes(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_list_sandboxes",
			Description: t("TOOL_E2B_LIST_SANDBOXES_DESCRIPTION", "List all active and paused E2B cloud sandboxes for your account. Lifecycle state machine: running -> paused -> destroyed. Best practice: Call this tool at the start of a session to discover reusable sandboxes, and at the end of a task to verify no billed VMs remain running."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_LIST_SANDBOXES_TITLE", "List E2B Sandboxes"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds before sandboxes are automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := `
import json, sys
from e2b import Sandbox
from datetime import datetime, timezone

try:
    pag = Sandbox.list()
    items = pag.next_items()
    now = datetime.now(timezone.utc)
    sbx_list = []
    for i in items:
        st_at = getattr(i, "started_at", None)
        uptime = 0
        created_str = ""
        if st_at:
            created_str = st_at.isoformat()
            uptime = int((now - st_at).total_seconds())
        state_str = str(getattr(i, "state", "running"))
        sbx_list.append({
            "sandbox_id": getattr(i, "sandbox_id", ""),
            "state": state_str,
            "created_at": created_str,
            "uptime_seconds": uptime,
            "template": getattr(i, "template_id", "")
        })
    res_payload = {
        "status": "success",
        "sandboxes": sbx_list
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
`

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if len(parsed.Sandboxes) == 0 {
				return utils.NewToolResultText("Discovered 0 E2B Cloud Sandboxes. No billed VMs are currently running."), nil, nil
			}

			resJSON, _ := json.MarshalIndent(map[string]any{
				"total_sandboxes": len(parsed.Sandboxes),
				"sandboxes":       parsed.Sandboxes,
			}, "", "  ")

			outText := fmt.Sprintf("Discovered %d E2B Cloud Sandboxes:\n\n%s", len(parsed.Sandboxes), string(resJSON))
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 4. E2BDesktopScreenshot: Desktop GUI screenshot
func E2BDesktopScreenshot(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_screenshot",
			Description: t("TOOL_E2B_DESKTOP_SCREENSHOT_DESCRIPTION", "Take a screenshot of official E2B Cloud Desktop GUI (Linux XFCE). Set 'keep_alive': true to persist the desktop sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_DESKTOP_SCREENSHOT_TITLE", "Take E2B Desktop Screenshot"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import base64, json, time
from e2b_desktop import Sandbox

sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    sbx.stream.start()
    vnc_url = sbx.stream.get_url()
    shot_bytes = sbx.screenshot()
    b64_str = base64.b64encode(shot_bytes).decode('utf-8')
    is_persistent = bool(sbx_id) or keep_alive or reused_existing

    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "vnc_url": vnc_url,
        "base64_length": len(b64_str),
        "data_prefix": f"data:image/png;base64,{b64_str[:100]}...",
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\nDesktop Screenshot Captured!\nLive Stream URL: %s\nData Prefix: %s",
				parsed.SandboxID, parsed.ReusedExisting, output, output)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 5. E2BDesktopClick: Desktop GUI click
func E2BDesktopClick(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_click",
			Description: t("TOOL_E2B_DESKTOP_CLICK_DESCRIPTION", "Perform mouse click at (x, y) on E2B Cloud Desktop GUI. Set 'keep_alive': true to persist the desktop sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_DESKTOP_CLICK_TITLE", "Click on E2B Desktop"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"x": {
						Type:        "integer",
						Description: "X coordinate (0-1024)",
					},
					"y": {
						Type:        "integer",
						Description: "Y coordinate (0-768)",
					},
					"action": {
						Type:        "string",
						Description: "Action: 'left', 'right', 'double', or 'middle' (default 'left')",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"x", "y"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			xf, err := RequiredParam[float64](args, "x")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			yf, err := RequiredParam[float64](args, "y")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			x, y := int(xf), int(yf)
			action, _ := OptionalParam[string](args, "action")
			if action == "" {
				action = "left"
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json
from e2b_desktop import Sandbox

sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
act = "%s"
x_coord = %d
y_coord = %d
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    if act == "right":
        sbx.right_click(x_coord, y_coord)
    elif act == "double":
        sbx.double_click(x_coord, y_coord)
    elif act == "middle":
        sbx.middle_click(x_coord, y_coord)
    else:
        sbx.left_click(x_coord, y_coord)

    is_persistent = bool(sbx_id) or keep_alive or reused_existing
    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "message": f"Executed {act} click at ({x_coord}, {y_coord})",
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), action, x, y, ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\n%s", parsed.SandboxID, parsed.ReusedExisting, parsed.Message)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 6. E2BDesktopType: Desktop GUI type
func E2BDesktopType(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_type",
			Description: t("TOOL_E2B_DESKTOP_TYPE_DESCRIPTION", "Type text or press keys on E2B Cloud Desktop GUI. Set 'keep_alive': true to persist the desktop sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_DESKTOP_TYPE_TITLE", "Type on E2B Desktop"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"text": {
						Type:        "string",
						Description: "Text string to type onto desktop",
					},
					"key": {
						Type:        "string",
						Description: "Key to press (e.g. 'enter', 'tab', 'backspace', 'ctrl', 'alt')",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			text, _ := OptionalParam[string](args, "text")
			key, _ := OptionalParam[string](args, "key")
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json
from e2b_desktop import Sandbox

text_to_type = %s
key_to_press = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    if text_to_type:
        sbx.write(text_to_type)
    if key_to_press:
        sbx.press(key_to_press)

    is_persistent = bool(sbx_id) or keep_alive or reused_existing
    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "message": "Desktop typing action executed successfully",
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(text), escapePyString(key), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\n%s", parsed.SandboxID, parsed.ReusedExisting, parsed.Message)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 7. E2BReadFile: Read file from sandbox
func E2BReadFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_read_file",
			Description: t("TOOL_E2B_READ_FILE_DESCRIPTION", "Read file contents from sandbox. Set 'keep_alive': true to persist the sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_READ_FILE_TITLE", "Read File from E2B Sandbox"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"path": {
						Type:        "string",
						Description: "Absolute file path inside the sandbox",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"path"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json
from e2b import Sandbox

path = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    content = sbx.files.read(path)
    is_persistent = bool(sbx_id) or keep_alive or reused_existing
    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "stdout": str(content),
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(path), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\n%s", parsed.SandboxID, parsed.ReusedExisting, parsed.Stdout)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 8. E2BWriteFile: Write file to sandbox
func E2BWriteFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_write_file",
			Description: t("TOOL_E2B_WRITE_FILE_DESCRIPTION", "Write text content to a file in sandbox. Set 'keep_alive': true to persist the sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_WRITE_FILE_TITLE", "Write File in E2B Sandbox"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"path": {
						Type:        "string",
						Description: "Absolute file path inside the sandbox",
					},
					"content": {
						Type:        "string",
						Description: "Text content to write to the file",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"path", "content"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			content, err := RequiredParam[string](args, "content")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json
from e2b import Sandbox

path = %s
content = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    sbx.files.write(path, content)
    is_persistent = bool(sbx_id) or keep_alive or reused_existing
    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "message": f"File successfully written to {path}",
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(path), escapePyString(content), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\n%s", parsed.SandboxID, parsed.ReusedExisting, parsed.Message)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 9. E2BListDir: List directory structure in sandbox
func E2BListDir(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_list_dir",
			Description: t("TOOL_E2B_LIST_DIR_DESCRIPTION", "List files and directory contents inside sandbox. Set 'keep_alive': true to persist the sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_LIST_DIR_TITLE", "List Directory in E2B Sandbox"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"path": {
						Type:        "string",
						Description: "Directory path (defaults to '/home/user')",
					},
					"sandbox_id": {
						Type:        "string",
						Description: "Existing E2B sandbox ID to reuse. Omitting it reuses the most recent active sandbox unless new_sandbox is true.",
					},
					"keep_alive": {
						Type:        "boolean",
						Description: "Keeps the sandbox running after this command instead of auto-destroying it. The sandbox bills for wall-clock uptime while alive. You must call e2b_kill_sandbox when finished.",
					},
					"new_sandbox": {
						Type:        "boolean",
						Description: "Set true to explicitly force creation of a new billed sandbox VM instead of reusing an existing active sandbox.",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Optional maximum allowed idle TTL in seconds (default 900 = 15 minutes) before the sandbox is automatically destroyed.",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, _ := OptionalParam[string](args, "path")
			if path == "" {
				path = "/home/user"
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			keepAlive, _ := OptionalParam[bool](args, "keep_alive")
			newSandbox, _ := OptionalParam[bool](args, "new_sandbox")
			ttlSecondsFloat, _ := OptionalParam[float64](args, "ttl_seconds")
			ttlSeconds := int(ttlSecondsFloat)

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			fallbackID := ""
			if sandboxID == "" && !newSandbox {
				fallbackID = getLastActiveSandboxID()
			}

			pyScript := fmt.Sprintf(`
import json
from e2b import Sandbox

path = %s
sbx_id = %s
fallback_sbx_id = %s
keep_alive = %s
new_sandbox = %s
ttl_seconds = %d
effective_ttl = ttl_seconds if ttl_seconds > 0 else 900

reused_existing = False
sbx = None

try:
    if sbx_id:
        try:
            sbx = Sandbox.connect(sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    elif not new_sandbox and fallback_sbx_id:
        try:
            sbx = Sandbox.connect(fallback_sbx_id)
            reused_existing = True
        except Exception:
            sbx = Sandbox.create(timeout=effective_ttl)
    else:
        sbx = Sandbox.create(timeout=effective_ttl)

    try:
        sbx.set_timeout(effective_ttl)
    except Exception:
        pass

    files = sbx.files.list(path)
    def _entry_kind(entry):
        raw = getattr(entry, "type", None)
        raw = getattr(raw, "value", raw)
        return "dir" if str(raw).lower().endswith("dir") else "file"

    res = [f"{f.name} ({_entry_kind(f)})" for f in files]
    is_persistent = bool(sbx_id) or keep_alive or reused_existing
    res_payload = {
        "status": "success",
        "sandbox_id": sbx.sandbox_id,
        "reused_existing": reused_existing,
        "stdout": "\n".join(res),
        "is_persistent": is_persistent
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
except Exception as infra_err:
    clean_msg = str(infra_err).split("\n")[0]
    res_payload = {
        "status": "infrastructure_error",
        "error": clean_msg,
        "sandbox_id": sbx.sandbox_id if sbx else ""
    }
    print("---E2B_JSON_START---")
    print(json.dumps(res_payload))
    print("---E2B_JSON_END---")
finally:
    if sbx and not (bool(sbx_id) or keep_alive or reused_existing):
        try:
            sbx.kill()
        except Exception:
            pass
`, escapePyString(path), escapePyString(sandboxID), escapePyString(fallbackID), pyBool(keepAlive), pyBool(newSandbox), ttlSeconds)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil || parsed.Status == "infrastructure_error" {
				errMsg := output
				if parsed != nil && parsed.Error != "" {
					errMsg = parsed.Error
				}
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %s", errMsg)), nil, nil
			}

			if parsed.IsPersistent {
				updateLastActiveSandboxID(parsed.SandboxID)
			} else {
				clearLastActiveSandboxID(parsed.SandboxID)
			}

			outText := fmt.Sprintf("[sandbox_id: %s] [reused_existing: %v]\n%s", parsed.SandboxID, parsed.ReusedExisting, parsed.Stdout)
			return utils.NewToolResultText(outText), nil, nil
		},
	)
}

// 10. E2BKillSandbox: Explicitly kill a persistent sandbox session when done
func E2BKillSandbox(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_kill_sandbox",
			Description: t("TOOL_E2B_KILL_SANDBOX_DESCRIPTION", "Explicitly terminate and destroy an active persistent E2B cloud sandbox VM by its sandbox_id. Lifecycle state machine: running -> paused -> destroyed. Returns success if target sandbox is already paused or destroyed."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_E2B_KILL_SANDBOX_TITLE", "Kill E2B Sandbox"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"sandbox_id": {
						Type:        "string",
						Description: "The E2B sandbox ID to terminate",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
				Required: []string{"sandbox_id"},
			},
		},
		[]scopes.Scope{},
		func(_ context.Context, _ ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			sandboxID, err := RequiredParam[string](args, "sandbox_id")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			clearLastActiveSandboxID(sandboxID)

			pyScript := fmt.Sprintf(`
import json
from e2b import Sandbox

sbx_id = %s
try:
    sbx = Sandbox.connect(sbx_id)
    sbx.kill()
    res_payload = {
        "status": "success",
        "sandbox_id": sbx_id,
        "message": f"Sandbox {sbx_id} successfully terminated and destroyed."
    }
except Exception as e:
    res_payload = {
        "status": "success",
        "sandbox_id": sbx_id,
        "message": f"Sandbox {sbx_id} is already paused, terminated, or not found."
    }
print("---E2B_JSON_START---")
print(json.dumps(res_payload))
print("---E2B_JSON_END---")
`, escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Infrastructure Error: %v", err)), nil, nil
			}

			parsed, parseErr := parseE2BJSONResponse(output)
			if parseErr != nil {
				return utils.NewToolResultText(fmt.Sprintf("Sandbox %s is terminated or not found.", sandboxID)), nil, nil
			}

			return utils.NewToolResultText(parsed.Message), nil, nil
		},
	)
}

func escapePyString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
