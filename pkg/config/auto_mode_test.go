package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAutoMode_UnmarshalYAMLAcceptsBoolAndString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		yaml string
		want string
	}{
		{"auto: true", "auto"},
		{"auto: false", ""},
		{"auto: on", "auto"},
		{"auto: off", ""},
		{"auto: auto", "auto"},
		{"auto: bug-hunt", "bug-hunt"},
		{`auto: "true"`, "auto"},
	}

	for _, tc := range cases {
		var doc struct {
			Auto *AutoMode `yaml:"auto"`
		}

		require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &doc), tc.yaml)
		require.NotNil(t, doc.Auto, tc.yaml)
		assert.Equal(t, tc.want, string(*doc.Auto), tc.yaml)
	}
}

func TestAutoMode_UnmarshalJSONAcceptsBoolAndString(t *testing.T) {
	t.Parallel()

	var enabled AutoMode
	require.NoError(t, enabled.UnmarshalJSON([]byte(`true`)))
	assert.Equal(t, "auto", string(enabled))

	var named AutoMode
	require.NoError(t, named.UnmarshalJSON([]byte(`"bug-hunt"`)))
	assert.Equal(t, "bug-hunt", string(named))

	var off AutoMode
	require.NoError(t, off.UnmarshalJSON([]byte(`false`)))
	assert.Empty(t, string(off))
}
