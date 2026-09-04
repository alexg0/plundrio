package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/reconcile"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type reconcileAPI interface {
	reconcile.Client
	Authenticate(context.Context) error
}

func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile Put.io and local download objects",
	}

	cmd.PersistentFlags().String("config", "", "Config file")
	cmd.PersistentFlags().StringP("target", "t", "", "Local download root (required)")
	cmd.PersistentFlags().StringP("folder", "f", "plundrio", "Put.io download folder name")
	cmd.PersistentFlags().StringP("token", "k", "", "Put.io OAuth token (required)")

	cmd.AddCommand(&cobra.Command{
		Use:   "report",
		Short: "Print a read-only JSON report of active and unmanaged objects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadReconcileConfig(cmd)
			if err != nil {
				return err
			}

			client := api.NewClient(cfg.token)
			folderID, err := resolveReconcileFolder(cmd.Context(), client, cfg.folder)
			if err != nil {
				return err
			}
			report, err := reconcile.New(client, folderID, cfg.target).Reconcile(cmd.Context())
			if err != nil {
				return err
			}
			return writeJSON(cmd, report)
		},
	})

	return cmd
}

type reconcileConfig struct {
	target string
	folder string
	token  string
}

func loadReconcileConfig(cmd *cobra.Command) (reconcileConfig, error) {
	v := viper.New()
	v.SetEnvPrefix("PLDR")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		return reconcileConfig{}, fmt.Errorf("bind reconciliation flags: %w", err)
	}

	configFile := v.GetString("config")
	if configFile != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return reconcileConfig{}, fmt.Errorf("read config file %q: %w", configFile, err)
		}
	}

	cfg := reconcileConfig{
		target: v.GetString("target"),
		folder: strings.ToLower(v.GetString("folder")),
		token:  v.GetString("token"),
	}
	if cfg.target == "" || cfg.folder == "" || cfg.token == "" {
		return reconcileConfig{}, fmt.Errorf("target, folder, and token are required")
	}
	if info, err := os.Stat(cfg.target); err != nil {
		return reconcileConfig{}, fmt.Errorf("stat local download root: %w", err)
	} else if !info.IsDir() {
		return reconcileConfig{}, fmt.Errorf("local download root %q is not a directory", cfg.target)
	}
	return cfg, nil
}

func resolveReconcileFolder(ctx context.Context, client reconcileAPI, folderName string) (int64, error) {
	if err := client.Authenticate(ctx); err != nil {
		return 0, err
	}
	files, err := client.GetFiles(ctx, 0)
	if err != nil {
		return 0, fmt.Errorf("list Put.io root: %w", err)
	}
	for _, file := range files {
		if file.Name == folderName && file.IsDir() {
			return file.ID, nil
		}
	}
	return 0, fmt.Errorf("Put.io folder %q does not exist", folderName)
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}

var _ reconcileAPI = (*api.Client)(nil)
