package main

import "testing"

func TestStalledTransferTimeoutFlag(t *testing.T) {
	flag := runCmd.Flags().Lookup("stalled-transfer-timeout")
	if flag == nil {
		t.Fatal("stalled-transfer-timeout flag is not registered")
	}
	if got, want := flag.DefValue, defaultStalledTransferTimeout.String(); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
}
