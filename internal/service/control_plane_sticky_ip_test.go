package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
)

func TestPlatformStickyReuseIPConfiguration(t *testing.T) {
	engine, closer, err := state.PersistenceBootstrap(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return time.Minute },
	})
	cp := &ControlPlaneService{Engine: engine, Pool: pool, EnvCfg: &config.EnvConfig{
		DefaultPlatformStickyTTL:              time.Hour,
		DefaultPlatformReverseProxyMissAction: "TREAT_AS_EMPTY",
		DefaultPlatformAllocationPolicy:       "BALANCED",
	}}
	name := "Grok"
	created, err := cp.CreatePlatform(CreatePlatformRequest{Name: &name})
	if err != nil || created.StickyReuseIP {
		t.Fatalf("default=%+v %v", created, err)
	}
	for _, enabled := range []bool{true, false, true} {
		patch, _ := json.Marshal(map[string]bool{"sticky_reuse_ip": enabled})
		updated, err := cp.UpdatePlatform(created.ID, patch)
		if err != nil || updated.StickyReuseIP != enabled {
			t.Fatalf("patch=%+v %v", updated, err)
		}
		stored, err := engine.GetPlatform(created.ID)
		if err != nil || stored.StickyReuseIP != enabled {
			t.Fatalf("stored=%+v %v", stored, err)
		}
		rebuilt, err := platform.BuildFromModel(*stored)
		if err != nil || rebuilt.StickyReuseIP != enabled {
			t.Fatalf("rebuilt=%+v %v", rebuilt, err)
		}
		live, _ := pool.GetPlatform(created.ID)
		if live.StickyReuseIP != enabled {
			t.Fatal("live setting not updated")
		}
	}
	for _, bad := range []string{`{"sticky_reuse_ip":null}`, `{"sticky_reuse_ip":"true"}`, `{"sticky_reuse_ip":1}`} {
		if _, err := cp.UpdatePlatform(created.ID, json.RawMessage(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	reset, err := cp.ResetPlatformToDefault(created.ID)
	if err != nil || reset.StickyReuseIP {
		t.Fatalf("reset=%+v %v", reset, err)
	}
	name = "GrokEnabled"
	enabled := true
	created, err = cp.CreatePlatform(CreatePlatformRequest{Name: &name, StickyReuseIP: &enabled})
	if err != nil || !created.StickyReuseIP {
		t.Fatalf("create enabled=%+v %v", created, err)
	}
}
