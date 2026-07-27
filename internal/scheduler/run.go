package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/harness9/internal/logfmt"
)

// RecoverInterrupted reconciles Attempts left running by a previous process
// that exited without a clean shutdown, marking them indeterminate for later
// reconciliation instead of silently retrying them. Call it once before Run's
// first Tick.
func (s *Scheduler) RecoverInterrupted(ctx context.Context) (int, error) {
	count, err := s.store.MarkInterruptedAttemptsIndeterminate(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted attempts: %w", err)
	}
	if count > 0 {
		log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("对账 %d 个中断 Attempt 为 indeterminate", count)))
	}
	return count, nil
}

// Run recovers interrupted Attempts once, then calls Tick on every interval
// until ctx is cancelled. Tick errors are logged, not returned, so one failing
// Task or Mission never stops the dispatch loop for the rest of the fleet.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) error {
	if _, err := s.RecoverInterrupted(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			dispatched, err := s.Tick(ctx)
			if err != nil {
				log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("tick 出错: %v", err)))
			}
			if dispatched > 0 {
				log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("本轮调度 %d 个 Task", dispatched)))
			}
		}
	}
}
