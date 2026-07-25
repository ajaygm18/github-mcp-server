package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ghErrors "github.com/github/github-mcp-server/pkg/errors"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/go-github/v89/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxPreviewRunes bounds how much of an edit string is echoed back in results.
const maxPreviewRunes = 80

// maxDiffLines bounds how many lines of each side of the diff are rendered.
const maxDiffLines = 40

// fileEdit is a single string replacement to apply to a file.
type fileEdit struct {
	OldString  string
	NewString  string
	ReplaceAll bool
}

// appliedEdit reports the outcome of one edit.
type appliedEdit struct {
	Index        int    `json:"index"`
	Replacements int    `json:"replacements"`
	OldString    string `json:"old_string"`
	NewString    string `json:"new_string"`
}

// EditFileResponse is the compact result returned by edit_file. It deliberately
// omits the full file contents so that response size scales with the size of
// the change rather than the size of the file.
type EditFileResponse struct {
	Owner       string        `json:"owner"`
	Repo        string        `json:"repo"`
	Path        string        `json:"path"`
	Branch      string        `json:"branch"`
	DryRun      bool          `json:"dry_run"`
	CommitSHA   string        `json:"commit_sha,omitempty"`
	CommitURL   string        `json:"commit_url,omitempty"`
	BytesBefore int           `json:"bytes_before"`
	BytesAfter  int           `json:"bytes_after"`
	Edits       []appliedEdit `json:"edits"`
	Diff        string        `json:"diff"`
}

// EditFile creates a tool that applies targeted string replacements to a file.
//
// The GitHub contents API only supports whole-file writes, which forces callers
// of create_or_update_file and push_files to re-send an entire file to change a
// single line. This tool performs the read-modify-write cycle server-side, so
// the caller only transmits the text that actually changes.
func EditFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name: "edit_file",
			Description: t("TOOL_EDIT_FILE_DESCRIPTION",
				"Apply targeted string replacements to an existing file in a GitHub repository and commit the result. "+
					"Prefer this over create_or_update_file when changing part of a file: you send only the text that changes, not the whole file. "+
					"Each edit replaces old_string with new_string. By default old_string must appear exactly once, which guards against ambiguous edits; "+
					"set replace_all to true for intentional find-and-replace. Include enough surrounding context in old_string to make it unique. "+
					"To delete text, pass an empty new_string. Use dry_run to preview the diff without committing. "+
					"The file's blob SHA is resolved automatically, so no SHA needs to be supplied."),
			Annotations: &mcp.ToolAnnotations{
				Title:           t("TOOL_EDIT_FILE_USER_TITLE", "Edit file"),
				ReadOnlyHint:    false,
				DestructiveHint: ToBoolPtr(false),
				IdempotentHint:  false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Path to the file to edit, relative to the repository root",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to read from and commit to. Defaults to the repository's default branch",
					},
					"message": {
						Type:        "string",
						Description: "Commit message. Required unless dry_run is true",
					},
					"edits": {
						Type:        "array",
						Description: "Ordered list of replacements to apply. Each edit is applied to the result of the previous one",
						Items: &jsonschema.Schema{
							Type: "object",
							Properties: map[string]*jsonschema.Schema{
								"old_string": {
									Type:        "string",
									Description: "Exact text to find, copied verbatim from the file including whitespace and indentation",
								},
								"new_string": {
									Type:        "string",
									Description: "Replacement text. Pass an empty string to delete the matched text",
								},
								"replace_all": {
									Type:        "boolean",
									Description: "Replace every occurrence instead of requiring exactly one match. Default false",
									Default:     json.RawMessage(`false`),
								},
							},
							Required: []string{"old_string", "new_string"},
						},
					},
					"dry_run": {
						Type:        "boolean",
						Description: "Validate the edits and return the diff without creating a commit. Default false",
						Default:     json.RawMessage(`false`),
					},
				},
				Required: []string{"owner", "repo", "path", "edits"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := OptionalParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := OptionalParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			dryRun, err := OptionalBoolParamWithDefault(args, "dry_run", false)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			edits, err := parseFileEdits(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if !dryRun && strings.TrimSpace(message) == "" {
				return utils.NewToolResultError("missing required parameter: message (required unless dry_run is true)"), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError("failed to get GitHub client"), nil, nil
			}

			// Resolve the default branch when the caller did not name one, so that
			// the read and the write always target the same ref.
			if branch == "" {
				repoInfo, repoResp, err := client.Repositories.Get(ctx, owner, repo)
				if err != nil {
					return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to get repository info", repoResp, err), nil, nil
				}
				branch = repoInfo.GetDefaultBranch()
			}

			fileContent, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: branch})
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to read file contents", resp, err), nil, nil
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			if fileContent == nil {
				return utils.NewToolResultError(fmt.Sprintf("path %q is a directory, not a file", path)), nil, nil
			}

			before, err := fileContent.GetContent()
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("failed to decode file contents: %s. The file may be binary, which edit_file does not support", err.Error())), nil, nil
			}

			after, applied, err := applyFileEdits(before, edits)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if after == before {
				return utils.NewToolResultError("edits produced no change to the file; nothing to commit"), nil, nil
			}

			response := EditFileResponse{
				Owner:       owner,
				Repo:        repo,
				Path:        path,
				Branch:      branch,
				DryRun:      dryRun,
				BytesBefore: len(before),
				BytesAfter:  len(after),
				Edits:       applied,
				Diff:        renderDiff(before, after),
			}

			if !dryRun {
				opts := &github.RepositoryContentFileOptions{
					Message: github.Ptr(message),
					Content: []byte(after),
					Branch:  github.Ptr(branch),
					SHA:     github.Ptr(fileContent.GetSHA()),
				}
				committed, writeResp, err := client.Repositories.UpdateFile(ctx, owner, repo, path, opts)
				if err != nil {
					return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to commit edited file", writeResp, err), nil, nil
				}
				if writeResp != nil && writeResp.Body != nil {
					defer func() { _ = writeResp.Body.Close() }()
				}
				if committed != nil && committed.Commit.SHA != nil {
					response.CommitSHA = committed.Commit.GetSHA()
					response.CommitURL = committed.Commit.GetHTMLURL()
				}
			}

			r, err := json.Marshal(response)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}
			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// parseFileEdits extracts and validates the edits array from tool arguments.
func parseFileEdits(args map[string]any) ([]fileEdit, error) {
	raw, ok := args["edits"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("missing required parameter: edits")
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("parameter edits must be an array of objects")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("parameter edits must contain at least one edit")
	}

	edits := make([]fileEdit, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edits[%d] must be an object", i)
		}
		oldString, ok := m["old_string"].(string)
		if !ok {
			return nil, fmt.Errorf("edits[%d].old_string is required and must be a string", i)
		}
		if oldString == "" {
			return nil, fmt.Errorf("edits[%d].old_string must not be empty", i)
		}
		// new_string may be absent or empty, which means deletion.
		newString, _ := m["new_string"].(string)
		replaceAll, _ := m["replace_all"].(bool)
		if oldString == newString {
			return nil, fmt.Errorf("edits[%d] is a no-op: old_string and new_string are identical", i)
		}
		edits = append(edits, fileEdit{OldString: oldString, NewString: newString, ReplaceAll: replaceAll})
	}
	return edits, nil
}

// applyFileEdits applies edits in order, validating occurrence counts. Each
// edit sees the result of the previous one.
func applyFileEdits(content string, edits []fileEdit) (string, []appliedEdit, error) {
	applied := make([]appliedEdit, 0, len(edits))
	for i, e := range edits {
		count := strings.Count(content, e.OldString)
		switch {
		case count == 0:
			return "", nil, fmt.Errorf(
				"edits[%d]: old_string not found in file. Copy it verbatim from the file, including whitespace, indentation and line endings. Searched for: %q",
				i, truncateForMessage(e.OldString))
		case count > 1 && !e.ReplaceAll:
			return "", nil, fmt.Errorf(
				"edits[%d]: old_string is ambiguous, found %d occurrences. Add surrounding context to make it unique, or set replace_all to true. Searched for: %q",
				i, count, truncateForMessage(e.OldString))
		}

		if e.ReplaceAll {
			content = strings.ReplaceAll(content, e.OldString, e.NewString)
		} else {
			content = strings.Replace(content, e.OldString, e.NewString, 1)
			count = 1
		}

		applied = append(applied, appliedEdit{
			Index:        i,
			Replacements: count,
			OldString:    truncateForMessage(e.OldString),
			NewString:    truncateForMessage(e.NewString),
		})
	}
	return content, applied, nil
}

// renderDiff produces a compact view of the changed region by trimming the
// common prefix and suffix of the two versions.
func renderDiff(before, after string) string {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")

	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix && b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", prefix+1, len(b)-suffix-prefix, prefix+1, len(a)-suffix-prefix)
	for i := prefix; i < len(b)-suffix; i++ {
		if i-prefix >= maxDiffLines {
			fmt.Fprintf(&sb, "-... (%d more removed lines)\n", len(b)-suffix-i)
			break
		}
		fmt.Fprintf(&sb, "-%s\n", b[i])
	}
	for i := prefix; i < len(a)-suffix; i++ {
		if i-prefix >= maxDiffLines {
			fmt.Fprintf(&sb, "+... (%d more added lines)\n", len(a)-suffix-i)
			break
		}
		fmt.Fprintf(&sb, "+%s\n", a[i])
	}
	return sb.String()
}

// truncateForMessage renders a snippet safe for inclusion in JSON output.
func truncateForMessage(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\t", "\\t")
	r := []rune(s)
	if len(r) > maxPreviewRunes {
		return string(r[:maxPreviewRunes]) + "..."
	}
	return string(r)
}
