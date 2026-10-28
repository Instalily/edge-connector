// Package yamlfile reads configuration files strictly: an unknown or
// misspelled key is an error, not a silently ignored setting.
package yamlfile

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Decode reads the YAML file at path into v.
func Decode(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
