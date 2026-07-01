// Package render generates a gobackup config document from parsed container
// models and a shared defaults profile. It has no project-local dependencies
// (other than the OptOut sentinel constant), so it is pure and unit-testable
// without Docker.
package render

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Profiles maps a profile name (e.g. "default") to its model-body template.
type Profiles map[string]map[string]any

// LoadProfiles reads the supervisor's defaults.yml. YAML anchors / merge keys
// inside the file are resolved by yaml.v3 at parse time, so operators can keep
// their existing &anchor / <<: definitions. A missing file is not an error
// (label-only mode); a malformed file IS, so the caller keeps its last-good config.
func LoadProfiles(path string) (Profiles, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Profiles{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read defaults %s: %w", path, err)
	}
	var p Profiles
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse defaults %s: %w", path, err)
	}
	if p == nil {
		p = Profiles{}
	}
	return p, nil
}
