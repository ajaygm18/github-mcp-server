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
	Description: "Official E2B Cloud Desktop Sandbox, VNC Stream, File Manager, and Code Interpreter tools for running Python, Shell, and Linux GUI Desktop automation",
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

var keyMap = map[string]string{
	"alt":          "Alt_L",
	"alt_left":     "Alt_L",
	"alt_right":    "Alt_R",
	"backspace":    "BackSpace",
	"caps_lock":    "Caps_Lock",
	"cmd":          "Super_L",
	"command":      "Super_L",
	"control":      "Control_L",
	"ctrl":         "Control_L",
	"del":          "Delete",
	"delete":       "Delete",
	"down":         "Down",
	"enter":        "Return",
	"esc":          "Escape",
	"escape":       "Escape",
	"home":         "Home",
	"left":         "Left",
	"page_down":    "Page_Down",
	"page_up":      "Page_Up",
	"right":        "Right",
	"shift":        "Shift_L",
	"space":        "space",
	"tab":          "Tab",
	"up":           "Up",
	"win":          "Super_L",
}

func mapKey(k string) string {
	lower := strings.ToLower(k)
	if mapped, ok := keyMap[lower]; ok {
		return mapped
	}
	return k
}

func getE2BAPIKey(args map[string]any) string {
	if apiKey, ok := args["api_key"].(string); ok && apiKey != "" {
		return apiKey
	}
	return os.Getenv("E2B_API_KEY")
}

// Executes command in E2B official "desktop" or "base" sandbox template
func executeE2BOfficialCommand(apiKey, templateID, command string) (string, string, string, error) {
	client := &http.Client{Timeout: 90 * time.Second}

	if templateID == "" {
		templateID = "desktop"
	}

	// 1. Create Sandbox using official templateID
	createReqBody, _ := json.Marshal(e2bCreateSandboxRequest{TemplateID: templateID})
	req, err := http.NewRequest("POST", "https://api.e2b.dev/sandboxes", bytes.NewBuffer(createReqBody))
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to create sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", "", "", fmt.Errorf("E2B API error (%d): %s", resp.StatusCode, string(body))
	}

	var sbxRes e2bCreateSandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbxRes); err != nil {
		return "", "", "", fmt.Errorf("failed to decode sandbox response: %w", err)
	}

	// Defer deleting sandbox after command completion
	defer func() {
		delReq, _ := http.NewRequest("DELETE", fmt.Sprintf("https://api.e2b.dev/sandboxes/%s", sbxRes.SandboxID), nil)
		delReq.Header.Set("X-API-Key", apiKey)
		delResp, err := client.Do(delReq)
		if err == nil {
			delResp.Body.Close()
		}
	}()

	// 2. Execute command via REST command endpoint
	cmdReqBody, _ := json.Marshal(e2bCommandRequest{Cmd: command})
	cmdURL := fmt.Sprintf("https://api.e2b.dev/sandboxes/%s/commands", sbxRes.SandboxID)
	cmdReq, err := http.NewRequest("POST", cmdURL, bytes.NewBuffer(cmdReqBody))
	if err != nil {
		return "", "", sbxRes.SandboxID, fmt.Errorf("failed to create command request: %w", err)
	}
	cmdReq.Header.Set("X-API-Key", apiKey)
	cmdReq.Header.Set("Content-Type", "application/json")

	cmdResp, err := client.Do(cmdReq)
	if err != nil {
		return "", "", sbxRes.SandboxID, fmt.Errorf("failed to execute command: %w", err)
	}
	defer cmdResp.Body.Close()

	body, err := io.ReadAll(cmdResp.Body)
	if err != nil {
		return "", "", sbxRes.SandboxID, fmt.Errorf("failed to read command response: %w", err)
	}

	if cmdResp.StatusCode != http.StatusOK && cmdResp.StatusCode != http.StatusCreated {
		return "", "", sbxRes.SandboxID, fmt.Errorf("E2B Command Error (%d): %s", cmdResp.StatusCode, string(body))
	}

	var res e2bCommandResponse
	if err := json.Unmarshal(body, &res); err == nil && (res.Stdout != "" || res.Stderr != "") {
		return res.Stdout, res.Stderr, sbxRes.SandboxID, nil
	}

	return string(body), "", sbxRes.SandboxID, nil
}

// 1. E2BRunCode: Code Interpreter execution
func E2BRunCode(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_run_code",
			Description: t("TOOL_E2B_RUN_CODE_DESCRIPTION", "Run Python or Shell code inside official E2B Code Interpreter cloud sandbox and return execution results."),
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

			lang, _ := OptionalParam[string](args, "language")
			if lang == "" {
				lang = "python"
			}

			apiKey := getE2BAPIKey(args)
			if apiKey == "" {
				return utils.NewToolResultError("E2B API Key is missing."), nil, nil
			}

			var fullCmd string
			if lang == "bash" || lang == "shell" {
				fullCmd = code
			} else {
				fullCmd = fmt.Sprintf("python3 -c %q", code)
			}

			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "base", fullCmd)
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

// 2. E2BRunCommand: Terminal execution
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

			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "base", command)
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

// 3. E2BDesktopScreenshot: Official e2b-dev/desktop screenshot implementation
func E2BDesktopScreenshot(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_screenshot",
			Description: t("TOOL_E2B_DESKTOP_SCREENSHOT_DESCRIPTION", "Take a screenshot of the official E2B Cloud Desktop GUI (Linux XFCE)."),
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

			// Uses e2b-dev/desktop exact screenshot command: scrot --pointer /tmp/screenshot.png
			cmd := "scrot --pointer /tmp/screenshot.png && base64 -w 0 /tmp/screenshot.png"
			stdout, stderr, sbxID, err := executeE2BOfficialCommand(apiKey, "desktop", cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Screenshot Failed: %v", err)), nil, nil
			}

			b64Str := strings.TrimSpace(stdout)
			if len(b64Str) == 0 {
				return utils.NewToolResultError("Screenshot failed: " + stderr), nil, nil
			}

			vncURL := fmt.Sprintf("https://6080-%s.e2b.app/vnc.html?autoconnect=true&resize=scale", sbxID)
			return utils.NewToolResultText(fmt.Sprintf("Desktop Screenshot Captured!\nLive Stream URL: %s\nBase64 Image Length: %d chars", vncURL, len(b64Str))), nil, nil
		},
	)
}

// 4. E2BDesktopClick: Official e2b-dev/desktop left/right/double click implementation
func E2BDesktopClick(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_click",
			Description: t("TOOL_E2B_DESKTOP_CLICK_DESCRIPTION", "Perform mouse clicks (left, right, double) at (x, y) on the E2B Cloud Desktop GUI."),
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

			// Uses e2b-dev/desktop exact xdotool command: xdotool mousemove --sync x y click
			var clickCmd string
			switch action {
			case "right":
				clickCmd = fmt.Sprintf("xdotool mousemove --sync %d %d click 3", x, y)
			case "double":
				clickCmd = fmt.Sprintf("xdotool mousemove --sync %d %d click --repeat 2 1", x, y)
			case "middle":
				clickCmd = fmt.Sprintf("xdotool mousemove --sync %d %d click 2", x, y)
			default:
				clickCmd = fmt.Sprintf("xdotool mousemove --sync %d %d click 1", x, y)
			}

			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "desktop", clickCmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Click Failed: %v", err)), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Executed %s click at (%d, %d). %s %s", action, x, y, stdout, stderr)), nil, nil
		},
	)
}

// 5. E2BDesktopType: Official e2b-dev/desktop typing and key press implementation
func E2BDesktopType(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataE2B,
		mcp.Tool{
			Name:        "e2b_desktop_type",
			Description: t("TOOL_E2B_DESKTOP_TYPE_DESCRIPTION", "Type text or press mapped keys (Return, Tab, BackSpace, Escape, Super_L) on E2B Cloud Desktop GUI."),
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

			// Uses e2b-dev/desktop exact xdotool command: xdotool type --delay 12 -- text / xdotool key
			var typeCmd string
			if text != "" {
				typeCmd += fmt.Sprintf("xdotool type --delay 12 -- %q; ", text)
			}
			if key != "" {
				typeCmd += fmt.Sprintf("xdotool key %q; ", mapKey(key))
			}

			if typeCmd == "" {
				return utils.NewToolResultError("Please provide 'text' or 'key' to type."), nil, nil
			}

			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "desktop", typeCmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Desktop Type Failed: %v", err)), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Typing Action Executed. %s %s", stdout, stderr)), nil, nil
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

			cmd := fmt.Sprintf("cat %q", path)
			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "base", cmd)
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

			b64Content := base64.StdEncoding.EncodeToString([]byte(content))
			cmd := fmt.Sprintf("mkdir -p $(dirname %q) && echo %q | base64 -d > %q", path, b64Content, path)

			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "base", cmd)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("Failed to write file: %v", err)), nil, nil
			}
			if stderr != "" {
				return utils.NewToolResultError(stderr), nil, nil
			}
			return utils.NewToolResultText(fmt.Sprintf("File written to %s (stdout: %s)", path, stdout)), nil, nil
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

			cmd := fmt.Sprintf("ls -la %q", path)
			stdout, stderr, _, err := executeE2BOfficialCommand(apiKey, "base", cmd)
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
