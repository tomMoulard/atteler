package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestLicenseConsistency checks that the LICENSE file, .goreleaser.yaml nfpm
// metadata, and README License section all agree on the project license.
// This prevents silent license drift between the repository and release
// artifacts (see https://github.com/tomMoulard/atteler/issues/141).
func TestLicenseConsistency(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	t.Run("LICENSE_file_is_MIT", func(t *testing.T) {
		t.Parallel()

		data, err := os.ReadFile(filepath.Join(root, "LICENSE"))
		require.NoError(t, err, "LICENSE file must be readable")

		assert.True(t, strings.HasPrefix(strings.TrimSpace(string(data)), "MIT License"),
			"LICENSE file must start with 'MIT License'")
	})

	t.Run("goreleaser_nfpm_license_matches", func(t *testing.T) {
		t.Parallel()

		data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
		require.NoError(t, err, ".goreleaser.yaml must be readable")

		var cfg goreleaserConfig
		require.NoError(t, yaml.Unmarshal(data, &cfg), ".goreleaser.yaml must be valid YAML")

		require.NotEmpty(t, cfg.NFPMs, ".goreleaser.yaml must have at least one nfpm entry")

		for _, nfpm := range cfg.NFPMs {
			assert.Equal(t, "MIT", nfpm.License,
				"nfpm license must be MIT, not %q (update .goreleaser.yaml)", nfpm.License)
		}
	})

	t.Run("goreleaser_nfpm_includes_license_file", func(t *testing.T) {
		t.Parallel()

		data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
		require.NoError(t, err, ".goreleaser.yaml must be readable")

		var cfg goreleaserConfig
		require.NoError(t, yaml.Unmarshal(data, &cfg), ".goreleaser.yaml must be valid YAML")

		require.NotEmpty(t, cfg.NFPMs, ".goreleaser.yaml must have at least one nfpm entry")

		for _, nfpm := range cfg.NFPMs {
			found := false

			for _, c := range nfpm.Contents {
				if c.Src == "LICENSE" {
					found = true

					break
				}
			}

			assert.True(t, found,
				"nfpm entry %q must include a contents entry with src: LICENSE", nfpm.ID)
		}
	})

	t.Run("README_license_section_references_LICENSE_file", func(t *testing.T) {
		t.Parallel()

		data, err := os.ReadFile(filepath.Join(root, "README.md"))
		require.NoError(t, err, "README.md must be readable")

		content := string(data)
		assert.Contains(t, content, "## License",
			"README must have a '## License' section")
		assert.Contains(t, content, "[`LICENSE`](LICENSE)",
			"README License section must link to the LICENSE file")
	})
}

// goreleaserConfig is a minimal subset of the GoReleaser config schema used
// only for license-drift checks.
type goreleaserConfig struct {
	NFPMs []nfpmConfig `yaml:"nfpms"`
}

type nfpmConfig struct {
	ID       string         `yaml:"id"`
	License  string         `yaml:"license"`
	Contents []nfpmContents `yaml:"contents"`
}

type nfpmContents struct {
	Src string `yaml:"src"`
	Dst string `yaml:"dst"`
}
