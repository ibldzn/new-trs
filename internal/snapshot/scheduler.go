package snapshot

import (
	"context"
	"log/slog"
	"time"

	"github.com/ibldzn/trs/internal/audit"
)

func RunScheduler(ctx context.Context, service *Service, interval time.Duration, refreshOnStart bool, logger *slog.Logger) {
	refresh := func(trigger Trigger) {
		refreshContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if _, err := service.Refresh(refreshContext, trigger, audit.Attribution{}); err != nil && ctx.Err() == nil && err != ErrRefreshInProgress {
			logger.ErrorContext(ctx, "current snapshot refresh failed", "trigger", trigger, "error", err)
		}
	}
	if refreshOnStart {
		refresh(TriggerStartup)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh(TriggerScheduled)
		}
	}
}
