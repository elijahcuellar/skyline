// skyline/internal/validate/validator.go
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"

	yaml "github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ValidateFromBytes validates a YAML document (provided as bytes) against a JSON Schema
// (also provided as bytes). This avoids temporary files and suits embedded schema
// or in-memory validation.
func ValidateFromBytes(schemaBytes []byte, yamlBytes []byte) error {
	// Unmarshal YAML into an intermediate structure.
	var intermediate any
	if err := yaml.Unmarshal(yamlBytes, &intermediate); err != nil {
		return fmt.Errorf("unmarshal yaml (from bytes): %w", err)
	}

	// Marshal the intermediate structure to JSON bytes to normalize map keys.
	jsonBytes, err := json.Marshal(intermediate)
	if err != nil {
		return fmt.Errorf("marshal intermediate to json (from bytes): %w", err)
	}

	// Unmarshal the JSON bytes into an interface{} that the jsonschema validator expects.
	var doc any
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return fmt.Errorf("unmarshal json for validation (from bytes): %w", err)
	}

	// Use an in-memory compiler resource to avoid file-based URIs.
	const inlineSchemaURI = "inline://schema.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(inlineSchemaURI, bytes.NewReader(schemaBytes)); err != nil {
		return fmt.Errorf("add inline schema resource: %w", err)
	}

	// Compile and validate
	schema, err := compiler.Compile(inlineSchemaURI)
	if err != nil {
		return fmt.Errorf("compile inline schema: %w", err)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("schema validation failed (from bytes): %w", err)
	}

	return nil
}
