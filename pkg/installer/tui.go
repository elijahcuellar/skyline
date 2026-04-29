package installer

// This file previously adapted the Installer type to the TUI runner.
// The TUI package was removed as part of the lightweight cleanup. Keep a
// minimal stub to preserve the public API surface: RunTUI simply returns an
// error indicating TUI is unavailable.

import (
	"context"
	"errors"
)

// RunTUI reports that the interactive TUI has been removed in this simplified build.
func RunTUI(inst *Installer, ctx context.Context) error {
	return errors.New("TUI is not available in this build")
}
