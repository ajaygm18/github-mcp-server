package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	Description: "E2B Cloud Sandbox Code Execution tools for running Python, JavaScript, and shell commands in isolated sandboxes",
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
	client := &http.Client{Timeout: 60 * time.Second}

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

	// 2. Execute command inside Sandbox via envd process API
	cmdReqBody, _ := json.Marshal(e2bCommandRequest{Cmd: command})
	cmdURL := fmt.Sprintf("https://%s-49999.e2b.dev/process", sbxRes.SandboxID)
	cmdReq, err := http.NewRequest("POST", cmdURL, bytes.NewBuffer(cmdReqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create command request: %w", err)
	}
	cmdReq.Header.Set("X-API-Key", apiKey)
	cmdReq.Header.Set("Content-Type", "application/json")

	cmdResp, err := client.Do(cmdReq)
	if err != nil {
		// Fallback to REST command execution if envd direct port is blocked
		return runE2BCommandFallback(client, apiKey, sbxRes.SandboxID, command)
	}
	defer cmdResp.Body.Close()

	if cmdResp.StatusCode != http.StatusOK {
		return runE2BCommandFallback(client, apiKey, sbxRes.SandboxID, command)
	}

	var cmdRes e2bCommandResponse
	if err := json.NewDecoder(cmdResp.Body).Decode(&cmdRes); err != nil {
		body, _ := io.ReadAll(cmdResp.Body)
		return string(body), "", nil
	}

	return cmdRes.Stdout, cmdRes.Stderr, nil
}

func runE2BCommandFallback(client *http.Client, apiKey, sandboxID, command string) (string, string, error) {
	cmdReqBody, _ := json.Marshal(e2bCommandRequest{Cmd: command})
	cmdURL := fmt.Sprintf("https://api.e2b.dev/sandboxes/%s/commands", sandboxID)
	cmdReq, err := http.NewRequest("POST", cmdURL, bytes.NewBuffer(cmdReqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create fallback request: %w", err)
	}
	cmdReq.Header.Set("X-API-Key", apiKey)
	cmdReq.Header.Set("Content-Type", "application/json")

	cmdResp, err := client.Do(cmdReq)
	if err != nil {
		return "", "", fmt.Errorf("failed to execute fallback command: %w", err)
	}
	defer cmdResp.Body.Close()

	body, err := io.ReadAll(cmdResp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response: %w", err)
	}

	if cmdResp.StatusCode != http.StatusOK && cmdResp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("E2B Command Error (%d): %s", cmdResp.StatusCode, string(body))
	}

	return string(body), "", nil
}

// E2BRunCode creates a tool to execute code inside an E2B sandbox.
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
				// Format Python execution string
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

// E2BRunCommand creates a tool to run terminal commands inside an E2B sandbox.
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
