package github

import (
	"testing"

	"github.com/github/github-mcp-server/internal/toolsnaps"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2BToolSnapshots(t *testing.T) {
	t.Parallel()

	tools := []inventory.ServerTool{
		E2BRunCode(translations.NullTranslationHelper),
		E2BRunCommand(translations.NullTranslationHelper),
		E2BListSandboxes(translations.NullTranslationHelper),
		E2BDesktopScreenshot(translations.NullTranslationHelper),
		E2BDesktopClick(translations.NullTranslationHelper),
		E2BDesktopType(translations.NullTranslationHelper),
		E2BReadFile(translations.NullTranslationHelper),
		E2BWriteFile(translations.NullTranslationHelper),
		E2BListDir(translations.NullTranslationHelper),
		E2BKillSandbox(translations.NullTranslationHelper),
	}

	for _, st := range tools {
		tool := st.Tool
		t.Run(tool.Name, func(t *testing.T) {
			assert.NotEmpty(t, tool.Name)
			assert.NotEmpty(t, tool.Description)
			assert.NotNil(t, tool.InputSchema)
			require.NoError(t, toolsnaps.Test(tool.Name, tool))
		})
	}
}
