package server

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIMatchesTheRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas struct {
				ErrorCode struct {
					Enum []string `yaml:"enum"`
				} `yaml:"ErrorCode"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			if method != "parameters" {
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	routes := map[string]bool{}
	for _, rt := range New(Deps{}).routes() {
		routes[rt.method+" "+rt.pattern] = true
	}
	routes["GET "+attentionRoute] = true
	if a, b := slices.Sorted(maps.Keys(routes)), slices.Sorted(maps.Keys(documented)); !slices.Equal(a, b) {
		t.Errorf("routes %v\nspec   %v", a, b)
	}

	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`http\.Status\w+, "([a-z_]+)"`).FindAllStringSubmatch(string(source), -1) {
		if !slices.Contains(spec.Components.Schemas.ErrorCode.Enum, m[1]) {
			t.Errorf("error code %q is not in docs/openapi.yaml", m[1])
		}
	}
}
