package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	Description: "E2B Cloud Sandbox, Desktop GUI, File Manager, and Code Execution tools for running Python, Shell, and GUI Desktop automation in isolated cloud sandboxes",
	Default:     true,
	Icon:        "terminal",
}

type e2bCreateSandboxRequest struct {
	TemplateID string `json:"templateID"`
}

type e2bCreateSandboxResponse struct {
	SandboxID  string `json:"sandboxID"`
	TemplateID string `json:"templateID"`
}

type e2bCommandRequest struct {
	Cmd string `json:"cmd"`
}

type e2bCommandResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func getE2BAPIKey(args map[string]any) string {
	if apiKey, ok := args["api_key"].(string); ok && apiKey != "" {
		return apiKey
	}
	return os.Getenv("E2B_API_KEY")
}

func executeE2BSandboxCommand(apiKey, command string) (string, string, error) {
	client := &http.Client{Timeout: 90 * time.Second}

	// 1. Create Sandbox
	createReqBody, _ := json.Marshal(e2bCreateSandboxRequest{TemplateID: "base"})
	req, err := http.NewRequest("POST", "https://api.e2b.dev/sandboxes", bytes.NewBuffer(createReqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to create sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("E2B API error (%d): %s", resp.StatusCode, string(body))
	}

	var sbxRes e2bCreateSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbxRes); err != nil {
		return "", "", fmt.Errorf("failed to decode sandbox response: %w", err)
	}

	// Defer deleting sandbox after execution
	defer func() {
		delReq, _ := http.NewRequest("DELETE", fmt.Sprintf("https://api.e2b.dev/sandboxes/%s", sbxRes.SandboxID), nil)
		delReq.Header.Set("X-API-Key", apiKey)
		delResp, err := client.Do(delReq)
		if err == nil {
			delResp.Body.Close()
		}
	}()

	// 2. Execute command via REST fallback or envd
	return runE2BCommandFallback(client, apiKey, sbxRes.SandboxID, command)
}

func runE2BCommandFallback(client *http.Client, apiKey, sandboxID, command string) (string, string, error) {
	cmdReqBody, _ := json.Marshal(e2bCommandRequest{Cmd: command})
	cmdURL := fmt.Sprintf("https://api.e2b.dev/sandboxes/%s/commands", sandboxID)
	cmdReq, err := http.NewRequest("POST", cmdURL, bytes.NewBuffer(cmdReqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create command request: %w", err)
	}
	cmdReq.Header.Set("X-API-Key", apiKey)
	cmdReq.Header.Set("Content-Type", "application/json")

	cmdResp, err := client.Do(cmdReq)
	if err != nil {
		return "", "", fmt.Errorf("failed to execute command: %w", err)
	}
	defer cmdResp.Body.Close()

	body, err := io.ReadAll(cmdResp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	if cmdResp.StatusCode != http.StatusOK && cmdResp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("E2B Command Error (%d): %s", cmdResp.StatusCode, string(body))
	}

	var res e2bCommandResponse
	if err := json.Unmarshal(body, &res); err == nil && (res.Stdout != "" || res.Stderr != "") {
		return res.Stdout, res.Stderr, nil
	}

	return string(body), "", nil
}

// 1. E2BRunCode: Execute Python or Shell scripts
func E2BRunCode(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_code",
			Description: t("TOOL_E2B_RUN_CODE_DESCRIPTION", "Run Python or Shell code inside a secure isolated E2B Cloud Sandbox and return stdout/stderr outputs."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_RUN_CODE_TITLE", "Run Code in E2B Sandbox"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"code": {
						Type:        "string",
						Description: "The Python or Shell code to execute in the E2B sandbox",
					},
					"language": {
						Type:        "string",
						Description: "Programming language: 'python' or 'bash' (default is 'python')",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key. If omitted, uses E2B_API_KEY environment variable.",
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

			lang, _ := OptionalParam[string](args, "language")
			if lang == "" {
				lang = "python"
			}

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing. Please provide 'api_key' parameter or set E2B_API_KEY environment variable."), nil, nil
			}

			var fullCmd string
			if lang == "bash" || lang == "shell" {
				fullCmd = code
			} else {
				fullCmd = fmt.Sprintf("python3 -c %q", code)
			}

			stdout, stderr, err := executeE2BSandboxCommand(apiKey, fullCmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Execution Failed: %v", err)), nil, nil
			}

			outMsg := stdout
			if stderr != "" {
				outMsg += "\nSTDERR:\n" + stderr
			}

			return utils.NewToolResultText(outMsg), nil, nil
		},
	)
}

// 2. E2BRunCommand: Run bash terminal commands
func E2BRunCommand(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_command",
			Description: t("TOOL_E2B_RUN_COMMAND_DESCRIPTION", "Run a bash terminal command inside an E2B cloud sandbox."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_RUN_COMMAND_TITLE", "Run Terminal Command in E2B"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"command": {
						Type:        "string",
						Description: "The terminal command to execute in the E2B cloud sandbox",
					},
					"api_key": {
						Type:        "string",
						Description: "Optional E2B API Key. If omitted, uses E2B_API_KEY environment variable.",
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
				return utils.NewToolResultError("E2B API Key is missing. Please provide 'api_key' parameter or set E2B_API_KEY environment variable."), nil, nil
			}

			stdout, stderr, err := executeE2BSandboxCommand(apiKey, command)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("E2B Command Failed: %v", err)), nil, nil
			}

			outMsg := stdout
			if stderr != "" {
				outMsg += "\nSTDERR:\n" + stderr
			}

			return utils.NewToolResultText(outMsg), nil, nil
		},
	)
}

// 3. E2BReadFile: Read file contents from sandbox
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
						Description: "Absolute file path inside the sandbox (e.g. '/home/user/data.json')",
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

			cmd := fmt.Sprintf("cat %q", path)
			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Failed to read file: %v", err)), nil, nil
			}
			if stderr != "" {
				return utils.NewToolResultError(stderr), nil, nil
			}
			return utils.NewToolResultText(stdout), nil, nil
		},
	)
}

// 4. E2BWriteFile: Write content to a file inside sandbox
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

			b64Content := base64.StdEncoding.EncodeToString([]byte(content))
			cmd := fmt.Sprintf("mkdir -p $(dirname %q) && echo %q | base64 -d > %q", path, b64Content, path)

			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Failed to write file: %v", err)), nil, nil
			}
			if stderr != "" {
				return utils.NewToolResultError(stderr), nil, nil
			}
			return utils.NewToolResultText(fmt.Sprintf("File successfully written to %s (stdout: %s)", path, stdout)), nil, nil
		},
	)
}

// 5. E2BListDir: List directory structure in sandbox
func E2BListDir(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_list_dir",
			Description: t("TOOL_E2B_LIST_DIR_DESCRIPTION", "List files and directories inside an E2B cloud sandbox."),
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

			cmd := fmt.Sprintf("ls -la %q", path)
			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Failed to list directory: %v", err)), nil, nil
			}
			if stderr != "" {
				return utils.NewToolResultText(fmt.Sprintf("%s\nSTDERR: %s", stdout, stderr)), nil, nil
			}
			return utils.NewToolResultText(stdout), nil, nil
		},
	)
}

// 6. E2BDesktopScreenshot: Capture Cloud Desktop GUI screenshot
func E2BDesktopScreenshot(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_screenshot",
			Description: t("TOOL_E2B_DESKTOP_SCREENSHOT_DESCRIPTION", "Take a screenshot of the virtual Xvfb Desktop GUI in the E2B cloud sandbox."),
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

			cmd := "sudo apt-get update -y >/dev/null 2>&1 && sudo apt-get install -y xvfb scrot >/dev/null 2>&1; Xvfb :99 -ac -screen 0 1280x1024x24 >/dev/null 2>&1 & sleep 1; DISPLAY=:99 scrot /tmp/screen.png; base64 -w 0 /tmp/screen.png"
			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Screenshot Failed: %v", err)), nil, nil
			}

			b64Str := strings.TrimSpace(stdout)
			if len(b64Str) == 0 {
				return utils.NewToolResultError("Screenshot captured empty image or failed. " + stderr), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Screenshot Captured! Base64 Image Length: %d characters.\nBase64 Data Prefix: data:image/png;base64,%s...", len(b64Str), b64Str[:min(len(b64Str), 100)])), nil, nil
		},
	)
}

// 7. E2BDesktopClick: Mouse click on Desktop GUI at (x, y)
func E2BDesktopClick(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_click",
			Description: t("TOOL_E2B_DESKTOP_CLICK_DESCRIPTION", "Perform a mouse click at specific (x, y) coordinates on the E2B Cloud Desktop GUI."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_CLICK_TITLE", "Click on E2B Desktop"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"x": {
						Type:        "integer",
						Description: "X coordinate (0-1280)",
					},
					"y": {
						Type:        "integer",
						Description: "Y coordinate (0-1024)",
					},
					"button": {
						Type:        "string",
						Description: "Click button: 'left', 'right', or 'double' (default is 'left')",
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
			btn, _ := OptionalParam[string](args, "button")
			if btn == "" {
				btn = "left"
			}

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			var clickCmd string
			if btn == "right" {
				clickCmd = fmt.Sprintf("xdotool mousemove %d %d click 3", x, y)
			} else if btn == "double" {
				clickCmd = fmt.Sprintf("xdotool mousemove %d %d click --repeat 2 1", x, y)
			} else {
				clickCmd = fmt.Sprintf("xdotool mousemove %d %d click 1", x, y)
			}

			cmd := fmt.Sprintf("sudo apt-get install -y xvfb xdotool >/dev/null 2>&1; Xvfb :99 -ac -screen 0 1280x1024x24 >/dev/null 2>&1 & sleep 1; DISPLAY=:99 %s", clickCmd)
			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Click Failed: %v", err)), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Successfully clicked at (%d, %d) [%s click]. Output: %s %s", x, y, btn, stdout, stderr)), nil, nil
		},
	)
}

// 8. E2BDesktopType: Type text onto Cloud Desktop GUI
func E2BDesktopType(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_type",
			Description: t("TOOL_E2B_DESKTOP_TYPE_DESCRIPTION", "Type text or press keys on the E2B Cloud Desktop GUI."),
			Annotations: &mcp.ToolAnnotations{
				Title: t("TOOL_E2B_DESKTOP_TYPE_TITLE", "Type on E2B Desktop"),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"text": {
						Type:        "string",
						Description: "Text to type onto the active window",
					},
					"key": {
						Type:        "string",
						Description: "Optional key press (e.g. 'Return', 'Tab', 'BackSpace', 'ctrl+c')",
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

			var typeCmd string
			if text != "" {
				typeCmd += fmt.Sprintf("xdotool type %q; ", text)
			}
			if key != "" {
				typeCmd += fmt.Sprintf("xdotool key %q; ", key)
			}

			if typeCmd == "" {
				return utils.NewToolResultError("Please provide either 'text' or 'key' to type."), nil, nil
			}

			cmd := fmt.Sprintf("sudo apt-get install -y xvfb xdotool >/dev/null 2>&1; Xvfb :99 -ac -screen 0 1280x1024x24 >/dev/null 2>&1 & sleep 1; DISPLAY=:99 %s", typeCmd)
			stdout, stderr, err := executeE2BSandboxCommand(apiKey, cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Type Failed: %v", err)), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Successfully executed typing action. Output: %s %s", stdout, stderr)), nil, nil
		},
	)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
