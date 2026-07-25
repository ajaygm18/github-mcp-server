package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	Description: "Official E2B Cloud Desktop Sandbox, VNC Stream, File Manager, and Code Interpreter tools supporting single-shot and persistent multi-command sandbox sessions",
	Default:     true,
	Icon:        "terminal",
}

func getE2BAPIKey(args map[string]any) string {
	if apiKey, ok := args["api_key"].(string); ok && apiKey != "" {
		return apiKey
	}
	return os.Getenv("E2B_API_KEY")
}

func runE2BPythonScript(apiKey, pyCode string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-c", pyCode)
	cmd.Env = append(os.Environ(), fmt.Sprintf("E2B_API_KEY=%s", apiKey))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		errStr := stderr.String()
		if errStr == "" {
			errStr = stdout.String()
		}
		return "", fmt.Errorf("python execution error: %v (details: %s)", err, errStr)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// 1. E2BRunCode: Code Interpreter execution (supports persistent sandbox_id)
func E2BRunCode(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_code",
			Description: t("TOOL_E2B_RUN_CODE_DESCRIPTION", "Run Python code inside official E2B Code Interpreter cloud sandbox. Pass optional 'sandbox_id' to run in an existing persistent sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_RUN_CODE_TITLE", "Run Code in E2B Sandbox"),
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
						Description: "Optional existing E2B sandbox ID to re-use for persistent multi-step sessions",
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			code, err := RequiredParam[string](args, "code")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_code_interpreter import Sandbox

code_to_run = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    execution = sbx.run_code(code_to_run)
    stdout_text = "".join([str(l) for l in execution.logs.stdout])
    stderr_text = "".join([str(l) for l in execution.logs.stderr])
    out = stdout_text
    if stderr_text:
        out += "\nSTDERR:\n" + stderr_text
    if not out and execution.results:
        out = str(execution.results)
    
    print(f"[sandbox_id: {sbx.sandbox_id}]\n" + out)
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(code), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Run Code Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 2. E2BRunCommand: Terminal command execution (supports persistent sandbox_id)
func E2BRunCommand(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_command",
			Description: t("TOOL_E2B_RUN_COMMAND_DESCRIPTION", "Run terminal commands inside an E2B cloud sandbox. Pass optional 'sandbox_id' to re-use an existing persistent sandbox session."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_RUN_COMMAND_TITLE", "Run Command in E2B"),
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
						Description: "Optional existing E2B sandbox ID for persistent multi-command sessions",
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			command, err := RequiredParam[string](args, "command")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

cmd_to_run = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    res = sbx.commands.run(cmd_to_run)
    out = res.stdout
    if res.stderr:
        out += "\nSTDERR:\n" + res.stderr
    print(f"[sandbox_id: {sbx.sandbox_id}]\n" + out)
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(command), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Run Command Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 3. E2BDesktopScreenshot: Desktop GUI screenshot (supports persistent sandbox_id)
func E2BDesktopScreenshot(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_screenshot",
			Description: t("TOOL_E2B_DESKTOP_SCREENSHOT_DESCRIPTION", "Take a screenshot of the official E2B Cloud Desktop GUI (Linux XFCE). Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_SCREENSHOT_TITLE", "Take E2B Desktop Screenshot"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"sandbox_id": {
						Type:        "string",
						Description: "Optional existing E2B desktop sandbox ID",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
import base64
from e2b_desktop import Sandbox

sbx_id = %s
if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    sbx.stream.start()
    vnc_url = sbx.stream.get_url()
    shot_bytes = sbx.screenshot()
    b64_str = base64.b64encode(shot_bytes).decode('utf-8')
    print(f"[sandbox_id: {sbx.sandbox_id}]\nDesktop Screenshot Captured!\nLive Stream URL: {vnc_url}\nBase64 Length: {len(b64_str)}\nData Prefix: data:image/png;base64,{b64_str[:100]}...")
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Screenshot Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 4. E2BDesktopClick: Desktop GUI click (supports persistent sandbox_id)
func E2BDesktopClick(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_click",
			Description: t("TOOL_E2B_DESKTOP_CLICK_DESCRIPTION", "Perform mouse click at (x, y) on E2B Cloud Desktop GUI. Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_CLICK_TITLE", "Click on E2B Desktop"),
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
						Description: "Optional existing E2B desktop sandbox ID",
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			x, err := RequiredParam[int](args, "x")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			y, err := RequiredParam[int](args, "y")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			action, _ := OptionalParam[string](args, "action")
			if action == "" {
				action = "left"
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_desktop import Sandbox

sbx_id = %s
if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    act = "%s"
    if act == "right":
        sbx.right_click(%d, %d)
    elif act == "double":
        sbx.double_click(%d, %d)
    elif act == "middle":
        sbx.middle_click(%d, %d)
    else:
        sbx.left_click(%d, %d)
    print(f"[sandbox_id: {sbx.sandbox_id}]\nSuccessfully executed {act} click at ({%d}, {%d})")
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(sandboxID), action, x, y, x, y, x, y, x, y, x, y)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Click Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 5. E2BDesktopType: Desktop GUI type (supports persistent sandbox_id)
func E2BDesktopType(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_type",
			Description: t("TOOL_E2B_DESKTOP_TYPE_DESCRIPTION", "Type text or press keys on E2B Cloud Desktop GUI. Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_TYPE_TITLE", "Type on E2B Desktop"),
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
						Description: "Optional existing E2B desktop sandbox ID",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			text, _ := OptionalParam[string](args, "text")
			key, _ := OptionalParam[string](args, "key")
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_desktop import Sandbox

text_to_type = %s
key_to_press = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    if text_to_type:
        sbx.write(text_to_type)
    if key_to_press:
        sbx.press(key_to_press)
    print(f"[sandbox_id: {sbx.sandbox_id}]\nDesktop typing action executed successfully.")
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(text), escapePyString(key), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Type Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 6. E2BReadFile: Read file from sandbox
func E2BReadFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_read_file",
			Description: t("TOOL_E2B_READ_FILE_DESCRIPTION", "Read file contents from sandbox. Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_READ_FILE_TITLE", "Read File from E2B Sandbox"),
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
						Description: "Optional existing E2B sandbox ID",
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    content = sbx.files.read(path)
    print(f"[sandbox_id: {sbx.sandbox_id}]\n" + str(content))
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(path), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Read File Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 7. E2BWriteFile: Write file to sandbox
func E2BWriteFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_write_file",
			Description: t("TOOL_E2B_WRITE_FILE_DESCRIPTION", "Write text content to a file in sandbox. Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_WRITE_FILE_TITLE", "Write File in E2B Sandbox"),
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
						Description: "Optional existing E2B sandbox ID",
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			content, err := RequiredParam[string](args, "content")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
content = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    sbx.files.write(path, content)
    print(f"[sandbox_id: {sbx.sandbox_id}]\nFile successfully written to {path}")
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(path), escapePyString(content), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Write File Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 8. E2BListDir: List directory structure in sandbox
func E2BListDir(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_list_dir",
			Description: t("TOOL_E2B_LIST_DIR_DESCRIPTION", "List files and directory contents inside sandbox. Pass optional 'sandbox_id' for persistent sessions."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_LIST_DIR_TITLE", "List Directory in E2B Sandbox"),
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
						Description: "Optional existing E2B sandbox ID",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, _ := OptionalParam[string](args, "path")
			if path == "" {
				path = "/home/user"
			}
			sandboxID, _ := OptionalParam[string](args, "sandbox_id")
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
sbx_id = %s

if sbx_id:
    try:
        sbx = Sandbox.connect(sbx_id)
    except Exception:
        sbx = Sandbox.create()
else:
    sbx = Sandbox.create()

try:
    files = sbx.files.list(path)
    res = [f"{f.name} ({'dir' if f.is_dir else 'file'})" for f in files]
    print(f"[sandbox_id: {sbx.sandbox_id}]\n" + "\n".join(res))
finally:
    if not sbx_id:
        sbx.kill()
`, escapePyString(path), escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B List Dir Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 9. E2BKillSandbox: Explicitly kill a persistent sandbox session when done
func E2BKillSandbox(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_kill_sandbox",
			Description: t("TOOL_E2B_KILL_SANDBOX_DESCRIPTION", "Explicitly terminate and destroy an active persistent E2B cloud sandbox VM by its sandbox_id."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_KILL_SANDBOX_TITLE", "Kill E2B Sandbox"),
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
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			sandboxID, err := RequiredParam[string](args, "sandbox_id")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

sbx_id = %s
try:
    sbx = Sandbox.connect(sbx_id)
    sbx.kill()
    print(f"Sandbox {sbx_id} successfully terminated and destroyed.")
except Exception as e:
    print(f"Sandbox {sbx_id} already killed or not found: {e}")
`, escapePyString(sandboxID))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Kill Sandbox Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

func escapePyString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
