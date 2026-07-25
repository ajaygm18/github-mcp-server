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
	Description: "Official E2B Cloud Desktop Sandbox, VNC Stream, File Manager, and Code Interpreter tools for running Python, Shell, and Linux GUI Desktop automation",
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

// 1. E2BRunCode: Code Interpreter execution using e2b_code_interpreter
func E2BRunCode(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_code",
			Description: t("TOOL_E2B_RUN_CODE_DESCRIPTION", "Run Python code inside official E2B Code Interpreter cloud sandbox and return execution results."),
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

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_code_interpreter import Sandbox

code_to_run = %s
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
    print(out)
finally:
    sbx.kill()
`, escapePyString(code))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Run Code Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 2. E2BRunCommand: Terminal command execution
func E2BRunCommand(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_command",
			Description: t("TOOL_E2B_RUN_COMMAND_DESCRIPTION", "Run terminal commands inside an E2B cloud sandbox."),
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

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

cmd_to_run = %s
sbx = Sandbox.create()
try:
    res = sbx.commands.run(cmd_to_run)
    out = res.stdout
    if res.stderr:
        out += "\nSTDERR:\n" + res.stderr
    print(out)
finally:
    sbx.kill()
`, escapePyString(command))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Run Command Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 3. E2BDesktopScreenshot: Official e2b-dev/desktop screenshot implementation
func E2BDesktopScreenshot(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_screenshot",
			Description: t("TOOL_E2B_DESKTOP_SCREENSHOT_DESCRIPTION", "Take a screenshot of the official E2B Cloud Desktop GUI (Linux XFCE) and return live noVNC stream URL."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_SCREENSHOT_TITLE", "Take E2B Desktop Screenshot"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key.",
					},
				},
			},
		},
		[]scopes.Scope{},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := `
import base64
from e2b_desktop import Sandbox

sbx = Sandbox.create()
try:
    sbx.stream.start()
    vnc_url = sbx.stream.get_url()
    shot_bytes = sbx.screenshot()
    b64_str = base64.b64encode(shot_bytes).decode('utf-8')
    print(f"Desktop Screenshot Captured!\nLive Stream URL: {vnc_url}\nBase64 Length: {len(b64_str)}\nData Prefix: data:image/png;base64,{b64_str[:100]}...")
finally:
    sbx.kill()
`

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Screenshot Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 4. E2BDesktopClick: Official e2b-dev/desktop click implementation
func E2BDesktopClick(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_click",
			Description: t("TOOL_E2B_DESKTOP_CLICK_DESCRIPTION", "Perform mouse click (left, right, double) at (x, y) on E2B Cloud Desktop GUI."),
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

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_desktop import Sandbox

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
    print(f"Successfully executed {act} click at ({%d}, {%d})")
finally:
    sbx.kill()
`, action, x, y, x, y, x, y, x, y, x, y)

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Click Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

// 5. E2BDesktopType: Official e2b-dev/desktop type implementation
func E2BDesktopType(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_type",
			Description: t("TOOL_E2B_DESKTOP_TYPE_DESCRIPTION", "Type text or press keys on E2B Cloud Desktop GUI."),
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
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b_desktop import Sandbox

text_to_type = %s
key_to_press = %s

sbx = Sandbox.create()
try:
    if text_to_type:
        sbx.write(text_to_type)
    if key_to_press:
        sbx.press(key_to_press)
    print("Desktop typing action executed successfully.")
finally:
    sbx.kill()
`, escapePyString(text), escapePyString(key))

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
			Description: t("TOOL_E2B_READ_FILE_DESCRIPTION", "Read the contents of a file inside an E2B cloud sandbox."),
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
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
sbx = Sandbox.create()
try:
    content = sbx.files.read(path)
    print(content)
finally:
    sbx.kill()
`, escapePyString(path))

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
			Description: t("TOOL_E2B_WRITE_FILE_DESCRIPTION", "Write text content to a file inside an E2B cloud sandbox."),
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
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
content = %s
sbx = Sandbox.create()
try:
    sbx.files.write(path, content)
    print(f"File successfully written to {path}")
finally:
    sbx.kill()
`, escapePyString(path), escapePyString(content))

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
			Description: t("TOOL_E2B_LIST_DIR_DESCRIPTION", "List files and directory contents inside an E2B cloud sandbox."),
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
			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			pyScript := fmt.Sprintf(`
from e2b import Sandbox

path = %s
sbx = Sandbox.create()
try:
    files = sbx.files.list(path)
    res = [f"{f.name} ({'dir' if f.is_dir else 'file'})" for f in files]
    print("\n".join(res))
finally:
    sbx.kill()
`, escapePyString(path))

			output, err := runE2BPythonScript(apiKey, pyScript)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B List Dir Error: %v", err)), nil, nil
			}

			return utils.NewToolResultText(output), nil, nil
		},
	)
}

func escapePyString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
