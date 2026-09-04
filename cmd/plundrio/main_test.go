package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestStalledTransferTimeoutConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config string
		flag   string
		want   time.Duration
	}{
		{
			name: "default",
			want: defaultStalledTransferTimeout,
		},
		{
			name:   "YAML override",
			config: "stalled-transfer-timeout: 12h\n",
			want:   12 * time.Hour,
		},
		{
			name:   "YAML disables detection",
			config: "stalled-transfer-timeout: 0\n",
			want:   0,
		},
		{
			name:   "flag overrides YAML",
			config: "stalled-transfer-timeout: 12h\n",
			flag:   "30m",
			want:   30 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := viper.New()
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Duration("stalled-transfer-timeout", defaultStalledTransferTimeout, "")

			if tt.config != "" {
				v.SetConfigType("yaml")
				if err := v.ReadConfig(strings.NewReader(tt.config)); err != nil {
					t.Fatalf("ReadConfig() error = %v", err)
				}
			}
			if tt.flag != "" {
				if err := flags.Set("stalled-transfer-timeout", tt.flag); err != nil {
					t.Fatalf("Set() error = %v", err)
				}
			}
			if err := v.BindPFlags(flags); err != nil {
				t.Fatalf("BindPFlags() error = %v", err)
			}

			if got := v.GetDuration("stalled-transfer-timeout"); got != tt.want {
				t.Errorf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}
