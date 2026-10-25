package state

import (
	"testing"

	"github.com/Resinat/Resin/internal/model"
)

func TestIPHistoryPersistsDeduplicatesAndResetSurvivesRestart(t *testing.T) {
	stateDir, cacheDir := t.TempDir(), t.TempDir()
	engine, closer, err := PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	entries := []model.IPHistory{{EgressIP: "192.0.2.1", AccountHash: "marker-a", FirstSeenNs: 1}, {EgressIP: "192.0.2.2", AccountHash: "marker-a", FirstSeenNs: 2}}
	if err := engine.SaveIPHistory(entries); err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveIPHistory(entries); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkIPHistoryInitialized(); err != nil {
		t.Fatal(err)
	}
	closer.Close()
	engine, closer, err = PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := engine.LoadIPHistory()
	if err != nil || len(got) != 2 {
		t.Fatalf("history=%+v err=%v", got, err)
	}
	if err := engine.DeleteIPHistory("192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	closer.Close()
	engine, closer, err = PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	got, err = engine.LoadIPHistory()
	if err != nil || len(got) != 1 || got[0].EgressIP != "192.0.2.2" {
		t.Fatalf("reset history=%+v err=%v", got, err)
	}
	initialized, err := engine.IPHistoryInitialized()
	if err != nil || !initialized {
		t.Fatalf("bootstrap marker lost: %v", err)
	}
}
