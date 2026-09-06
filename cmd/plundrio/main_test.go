package main

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestStalledTransferTimeoutFlagWiring(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	flag := runCmd.Flags().Lookup("stalled-transfer-timeout")
	if flag == nil {
		t.Fatal("stalled-transfer-timeout flag is not registered")
	}

	originalValue, originalChanged := flag.Value.String(), flag.Changed
	t.Cleanup(func() {
		if err := flag.Value.Set(originalValue); err != nil {
			t.Errorf("restore stalled-transfer-timeout flag: %v", err)
		}
		flag.Changed = originalChanged
	})

	if got, want := flag.DefValue, defaultStalledTransferTimeout.String(); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	if err := runCmd.Flags().Set("stalled-transfer-timeout", "30m"); err != nil {
		t.Fatalf("set stalled-transfer-timeout flag: %v", err)
	}
	if err := viper.BindPFlags(runCmd.Flags()); err != nil {
		t.Fatalf("bind run command flags: %v", err)
	}

	if got, want := configuredStalledTransferTimeout(), 30*time.Minute; got != want {
		t.Errorf("configured timeout = %s, want %s", got, want)
	}
}
