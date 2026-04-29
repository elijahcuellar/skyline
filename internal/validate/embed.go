package validate

import _ "embed"

//go:embed install.schema.json
var embeddedSchema []byte

// EmbeddedSchemaBytes returns the embedded schema bytes.
func EmbeddedSchemaBytes() []byte { return embeddedSchema }

// EmbeddedHeaderBytes returns a small default header to print on startup.
func EmbeddedHeaderBytes() []byte { return []byte("Skyline Installer\n") }
