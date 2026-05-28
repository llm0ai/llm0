package workers

import (
	"context"
	"fmt"
	"time"

	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// Scheduler manages all scheduled background jobs
type Scheduler struct {
	db    *database.DB
	redis *redis.Client
}

// NewScheduler creates a new Scheduler instance
func NewScheduler(db *database.DB, redis *redis.Client) *Scheduler {
	return &Scheduler{
		db:    db,
		redis: redis,
	}
}

// StartAll starts all scheduled workers
func (s *Scheduler) StartAll(ctx context.Context) {
	fmt.Println("🚀 Starting all scheduled jobs...")

	// Hourly jobs
	go s.runHourly(ctx, "reconciliation", s.reconcileCustomerSpend)
	go s.runHourly(ctx, "cache-cleanup", s.cleanupExpiredCache)

	// Daily jobs
	go s.runDaily(ctx, 2, 0, "semantic-cache-cleanup", s.cleanupSemanticCache)

	// Weekly jobs
	go s.runWeekly(ctx, time.Sunday, 3, 0, "log-cleanup", s.cleanupOldLogs)

	// Monthly jobs
	go s.runMonthly(ctx, 1, 0, 0, "spend-reset", s.resetMonthlySpend)

	fmt.Println("✅ All scheduled jobs initialized")
}

// runHourly runs a job every hour at :00
func (s *Scheduler) runHourly(ctx context.Context, name string, job func(context.Context) error) {
	// Wait until next hour
	now := time.Now()
	nextRun := now.Truncate(time.Hour).Add(time.Hour)
	initialWait := nextRun.Sub(now)

	fmt.Printf("⏰ [%s] Scheduled hourly, first run in %s\n", name, initialWait.Round(time.Second))

	select {
	case <-time.After(initialWait):
		// First run
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		start := time.Now()
		fmt.Printf("🔄 [%s] Starting...\n", name)

		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := job(jobCtx)
		cancel()

		duration := time.Since(start)
		if err != nil {
			fmt.Printf("❌ [%s] Failed after %s: %v\n", name, duration.Round(time.Millisecond), err)
		} else {
			fmt.Printf("✅ [%s] Completed in %s\n", name, duration.Round(time.Millisecond))
		}

		select {
		case <-ticker.C:
			// Next iteration
		case <-ctx.Done():
			fmt.Printf("🛑 [%s] Stopped\n", name)
			return
		}
	}
}

// runDaily runs a job daily at a specific hour:minute UTC
func (s *Scheduler) runDaily(ctx context.Context, hour, minute int, name string, job func(context.Context) error) {
	fmt.Printf("⏰ [%s] Scheduled daily at %02d:%02d UTC\n", name, hour, minute)

	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}

		waitDuration := next.Sub(now)
		fmt.Printf("⏰ [%s] Next run in %s\n", name, waitDuration.Round(time.Second))

		select {
		case <-time.After(waitDuration):
			// Time to run
		case <-ctx.Done():
			fmt.Printf("🛑 [%s] Stopped\n", name)
			return
		}

		start := time.Now()
		fmt.Printf("🔄 [%s] Starting...\n", name)

		jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := job(jobCtx)
		cancel()

		duration := time.Since(start)
		if err != nil {
			fmt.Printf("❌ [%s] Failed after %s: %v\n", name, duration.Round(time.Millisecond), err)
		} else {
			fmt.Printf("✅ [%s] Completed in %s\n", name, duration.Round(time.Millisecond))
		}
	}
}

// runWeekly runs a job weekly on a specific day and time
func (s *Scheduler) runWeekly(ctx context.Context, weekday time.Weekday, hour, minute int, name string, job func(context.Context) error) {
	fmt.Printf("⏰ [%s] Scheduled weekly on %s at %02d:%02d UTC\n", name, weekday, hour, minute)

	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)

		// Find next occurrence of the weekday
		for next.Weekday() != weekday || now.After(next) {
			next = next.Add(24 * time.Hour)
		}

		waitDuration := next.Sub(now)
		fmt.Printf("⏰ [%s] Next run in %s\n", name, waitDuration.Round(time.Second))

		select {
		case <-time.After(waitDuration):
			// Time to run
		case <-ctx.Done():
			fmt.Printf("🛑 [%s] Stopped\n", name)
			return
		}

		start := time.Now()
		fmt.Printf("🔄 [%s] Starting...\n", name)

		jobCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		err := job(jobCtx)
		cancel()

		duration := time.Since(start)
		if err != nil {
			fmt.Printf("❌ [%s] Failed after %s: %v\n", name, duration.Round(time.Millisecond), err)
		} else {
			fmt.Printf("✅ [%s] Completed in %s\n", name, duration.Round(time.Millisecond))
		}
	}
}

// runMonthly runs a job on the Nth day of each month at hour:minute UTC
func (s *Scheduler) runMonthly(ctx context.Context, day, hour, minute int, name string, job func(context.Context) error) {
	fmt.Printf("⏰ [%s] Scheduled monthly on day %d at %02d:%02d UTC\n", name, day, hour, minute)

	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), day, hour, minute, 0, 0, time.UTC)
		if now.After(next) {
			next = next.AddDate(0, 1, 0) // Next month
		}

		waitDuration := next.Sub(now)
		fmt.Printf("⏰ [%s] Next run in %s\n", name, waitDuration.Round(time.Second))

		select {
		case <-time.After(waitDuration):
			// Time to run
		case <-ctx.Done():
			fmt.Printf("🛑 [%s] Stopped\n", name)
			return
		}

		start := time.Now()
		fmt.Printf("🔄 [%s] Starting...\n", name)

		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := job(jobCtx)
		cancel()

		duration := time.Since(start)
		if err != nil {
			fmt.Printf("❌ [%s] Failed after %s: %v\n", name, duration.Round(time.Millisecond), err)
		} else {
			fmt.Printf("✅ [%s] Completed in %s\n", name, duration.Round(time.Millisecond))
		}
	}
}
