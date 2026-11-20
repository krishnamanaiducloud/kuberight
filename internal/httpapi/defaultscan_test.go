package httpapi

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
)

func defaultScanConfig() config.Config {
	cfg := fallbackConfig()
	cfg.OperationMode, cfg.DefaultScanInterval, cfg.RecommendationWindow = config.ModeRecommend, 6*time.Hour, 7*24*time.Hour
	return cfg
}

func schedules(t *testing.T, store repository.Store) []repository.FleetSchedule {
	t.Helper()
	result, err := store.ListFleetSchedules(context.Background(), repository.FleetListOptions{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	return result.Items
}

func TestDefaultScanScheduleIsCreatedOnceAndScansSoon(t *testing.T) {
	store := repository.NewMemory(100)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 2 {
		if err := EnsureDefaultScanSchedule(context.Background(), defaultScanConfig(), store, logger, fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	items := schedules(t, store)
	if len(items) != 1 {
		t.Fatalf("schedules = %+v", items)
	}
	schedule := items[0]
	if !schedule.Enabled || schedule.IntervalSeconds != 6*3600 || schedule.WindowSeconds != 7*24*3600 ||
		!schedule.NextRunAt.Equal(fixedNow) || len(schedule.Scope.Namespaces) != 0 || schedule.CreatedBy != "kuberight" {
		t.Fatalf("schedule = %+v", schedule)
	}
	audit, err := store.ListAuditEvents(context.Background(), 1, 10)
	if err != nil || audit.Total != 1 || audit.Items[0].Resource != "fleet-schedules/"+schedule.ID {
		t.Fatalf("audit = %+v err=%v", audit, err)
	}
}

func TestDefaultScanScheduleLeavesOperatorsInCharge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, test := range map[string]struct {
		mutate   func(*config.Config)
		existing bool
	}{
		"disabled":          {func(cfg *config.Config) { cfg.DefaultScanInterval = 0 }, false},
		"observe mode":      {func(cfg *config.Config) { cfg.OperationMode = config.ModeObserve }, false},
		"existing schedule": {func(*config.Config) {}, true},
	} {
		store := repository.NewMemory(100)
		if test.existing {
			if _, err := store.CreateFleetSchedule(context.Background(), repository.FleetScheduleMutation{
				Name: "nightly", Enabled: false, IntervalSeconds: 86_400, WindowSeconds: 3600, NextRunAt: fixedNow, Actor: "alice",
			}); err != nil {
				t.Fatal(err)
			}
		}
		cfg := defaultScanConfig()
		test.mutate(&cfg)
		if err := EnsureDefaultScanSchedule(context.Background(), cfg, store, logger, fixedNow); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := 0
		if test.existing {
			want = 1
		}
		if items := schedules(t, store); len(items) != want || (want == 1 && items[0].Name != "nightly") {
			t.Fatalf("%s: schedules = %+v", name, items)
		}
	}
}

func TestDefaultScanIntervalSetting(t *testing.T) {
	cfg := validConfigForTest()
	cfg.DefaultScanInterval = time.Minute
	if err := cfg.Validate(); err == nil {
		t.Fatal("a scan interval below five minutes was accepted")
	}
	cfg.DefaultScanInterval = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero disables the default scan: %v", err)
	}
}

func validConfigForTest() config.Config {
	return fallbackConfig()
}
