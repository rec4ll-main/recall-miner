package handlers

import "testing"

func TestInventoryMinerVersionUsesLinkedCommit(t *testing.T) {
	t.Setenv("MINER_VERSION", "")
	t.Setenv("MINER_PUBLIC_VERSION", "")
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = "4efe527"
	if got := inventoryMinerVersion(); got != "recall-miner/4efe527" {
		t.Fatalf("got %q", got)
	}
	Version = ""
	if got := inventoryMinerVersion(); got != "recall-miner/dev" {
		t.Fatalf("got %q", got)
	}
}
