package mcp

import (
	"reflect"
	"strings"
	"testing"
)

func TestManifest_ValidateAcceptsValidServers(t *testing.T) {
	t.Parallel()

	manifest := Manifest{Servers: []Server{
		{
			Name:         "filesystem",
			Command:      "mcp-filesystem",
			Args:         []string{"/tmp"},
			Env:          map[string]string{"LOG_LEVEL": "debug"},
			Capabilities: []string{"read", "write"},
		},
		{Name: "git", Command: "mcp-git", Capabilities: []string{"repo"}},
	}}

	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestManifest_ValidateRejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	assertValidateError := func(t *testing.T, name string, manifest Manifest, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := manifest.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}

			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate() error = %q, want containing %q", err.Error(), want)
			}
		})
	}

	assertValidateError(t,
		"missing server name",
		Manifest{Servers: []Server{{Command: "mcp"}}},
		"server 0: missing name",
	)
	assertValidateError(t,
		"blank server name",
		Manifest{Servers: []Server{{Name: " \t", Command: "mcp"}}},
		"server 0: missing name",
	)
	assertValidateError(t,
		"missing command",
		Manifest{Servers: []Server{{Name: "filesystem"}}},
		`server "filesystem": missing command`,
	)
	assertValidateError(t,
		"blank command",
		Manifest{Servers: []Server{{Name: "filesystem", Command: " \t"}}},
		`server "filesystem": missing command`,
	)
	assertValidateError(t,
		"empty capability",
		Manifest{Servers: []Server{{Name: "filesystem", Command: "mcp", Capabilities: []string{"read", " "}}}},
		`server "filesystem": capability 1: empty capability`,
	)
}

func TestManifest_ValidateRejectsDuplicateNames(t *testing.T) {
	t.Parallel()

	manifest := Manifest{Servers: []Server{
		{Name: "git", Command: "mcp-git"},
		{Name: " git ", Command: "other-git"},
	}}

	err := manifest.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want duplicate error")
	}

	if !strings.Contains(err.Error(), `duplicate server name "git"`) {
		t.Fatalf("Validate() error = %q, want duplicate server name", err.Error())
	}
}

func TestManifest_ListReturnsSortedNames(t *testing.T) {
	t.Parallel()

	manifest := Manifest{Servers: []Server{
		{Name: "zeta", Command: "z"},
		{Name: " alpha ", Command: "a"},
		{Name: "middle", Command: "m"},
	}}

	got := manifest.List()

	want := []string{"alpha", "middle", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
}

func TestManifest_FindByCapability(t *testing.T) {
	t.Parallel()

	manifest := Manifest{Servers: []Server{
		{Name: "zeta", Command: "z", Capabilities: []string{"search", "write"}},
		{Name: "alpha", Command: "a", Capabilities: []string{" read ", "search"}},
		{Name: "middle", Command: "m", Capabilities: []string{"read"}},
	}}

	matches := manifest.Find(" search ")
	got := serverNames(matches)

	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Find() names = %#v, want %#v", got, want)
	}

	if matches[0].Command != "a" || matches[1].Command != "z" {
		t.Fatalf("Find() matches = %#v, want original server data", matches)
	}
}

func TestManifest_FindEmptyOrMissingCapability(t *testing.T) {
	t.Parallel()

	manifest := Manifest{Servers: []Server{{Name: "alpha", Command: "a", Capabilities: []string{"read"}}}}

	if got := manifest.Find(" "); got != nil {
		t.Fatalf("Find(blank) = %#v, want nil", got)
	}

	if got := manifest.Find("write"); got != nil {
		t.Fatalf("Find(missing) = %#v, want nil", got)
	}
}

func serverNames(servers []Server) []string {
	if len(servers) == 0 {
		return nil
	}

	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, strings.TrimSpace(server.Name))
	}

	return names
}
