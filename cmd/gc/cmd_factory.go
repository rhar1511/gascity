package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/factory"
	"github.com/spf13/cobra"
)

func newFactoryCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "factory",
		Short: "Inspect software-factory policy",
	}
	cmd.AddCommand(newFactoryValidateCmd(stdout, stderr))
	return cmd
}

func newFactoryValidateCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "validate [config-path]",
		Short: "Validate .agent-factory/config.yaml without running it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := factoryConfigPath(args)
			if err != nil {
				fmt.Fprintf(stderr, "gc factory validate: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := factory.LoadFile(path)
			if err != nil {
				fmt.Fprintf(stderr, "gc factory validate: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if jsonOutput {
				result := struct {
					OK           bool   `json:"ok"`
					ConfigPath   string `json:"config_path"`
					Repositories int    `json:"repositories"`
					Sources      int    `json:"sources"`
				}{true, path, len(cfg.Repositories), len(cfg.Sources)}
				if err := json.NewEncoder(stdout).Encode(result); err != nil {
					return err
				}
				return nil
			}
			fmt.Fprintf(stdout, "software factory config valid: %s (%d repositories, %d sources)\n", path, len(cfg.Repositories), len(cfg.Sources)) //nolint:errcheck // command output
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit structured JSON")
	return cmd
}

func factoryConfigPath(args []string) (string, error) {
	if len(args) == 1 {
		path, err := filepath.Abs(args[0])
		if err != nil {
			return "", err
		}
		return path, nil
	}
	city, err := resolveCity()
	if err != nil {
		return "", err
	}
	return filepath.Join(city, factory.ConfigRelativePath), nil
}
