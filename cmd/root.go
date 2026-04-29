package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/elijahcuellar/skyline/internal/validate"
	"github.com/elijahcuellar/skyline/pkg/installer"
)

var (
	cfgFile string
	tui     bool
)

var rootCmd = &cobra.Command{
	Use:   "skyline",
	Short: "Skyline installer CLI",
	Long:  "Apply system configuration from an install.yaml manifest (files, dnf, flatpak, bash).",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Default config file
		if cfgFile == "" {
			cfgFile = "install.yaml"
		}
		// Print embedded header if present
		hdr := string(validate.EmbeddedHeaderBytes())
		if hdr != "" {
			fmt.Println(hdr)
		}

		// Resolve config path
		absCfg, err := filepath.Abs(cfgFile)
		if err != nil {
			return fmt.Errorf("resolving config path: %w", err)
		}

		// Validate config against embedded JSON Schema
		schemaBytes := validate.EmbeddedSchemaBytes()
		yamlBytes, err := os.ReadFile(absCfg)
		if err != nil {
			return fmt.Errorf("read config for validation: %w", err)
		}
		if err := validate.ValidateFromBytes(schemaBytes, yamlBytes); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}

		// Load installer from already-read bytes (avoid double-read)
		inst, err := installer.NewFromBytes(yamlBytes)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		// Create cancellable context (60m)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()

		// Run interactive TUI if requested
		if tui {
			if err := installer.RunTUI(inst, ctx); err != nil {
				return fmt.Errorf("apply failed: %w", err)
			}
			// Success
			fmt.Println("Done — configuration applied successfully.")
			return nil
		}

		// Apply non-interactive
		if err := inst.Apply(ctx); err != nil {
			return fmt.Errorf("apply failed: %w", err)
		}

		fmt.Println("Done — configuration applied successfully.")
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "file", "f", "", "path to install.yaml (default ./install.yaml)")
	// Enable interactive TUI (streams progress).
	rootCmd.PersistentFlags().BoolVarP(&tui, "tui", "t", false, "run interactive TUI")
	// TODO: add future flags
}

// Execute root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Print error and exit
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
