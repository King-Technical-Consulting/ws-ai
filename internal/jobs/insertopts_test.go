package jobs

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// TestInsertOptsAccepted inserts each run job kind through a real River
// client: River validates InsertOpts at insert time, not at compile time
// (UniqueOpts.ByState must include the running state). agent.run is
// deduplicated per run; agent.resume is not. Needs a Postgres:
// WS_TEST_RIVER_DSN=postgres://... go test ./internal/jobs/ -run InsertOpts
func TestInsertOptsAccepted(t *testing.T) {
	dsn := os.Getenv("WS_TEST_RIVER_DSN")
	if dsn == "" {
		t.Skip("WS_TEST_RIVER_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	drv := riverpgxv5.New(pool)
	m, err := rivermigrate.New(drv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	c, err := river.NewClient(drv, &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	r1, err := c.Insert(ctx, RunArgs{RunID: id}, nil)
	if err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	r2, err := c.Insert(ctx, RunArgs{RunID: id}, nil)
	if err != nil {
		t.Fatalf("agent.run again: %v", err)
	}
	if !r2.UniqueSkippedAsDuplicate || r2.Job.ID != r1.Job.ID {
		t.Errorf("second agent.run for one run was not deduplicated: %+v", r2)
	}
	s1, err := c.Insert(ctx, ResumeArgs{RunID: id}, nil)
	if err != nil {
		t.Fatalf("agent.resume: %v", err)
	}
	s2, err := c.Insert(ctx, ResumeArgs{RunID: id}, nil)
	if err != nil {
		t.Fatalf("agent.resume again: %v", err)
	}
	if s1.UniqueSkippedAsDuplicate || s2.UniqueSkippedAsDuplicate || s1.Job.ID == s2.Job.ID {
		t.Errorf("agent.resume must not deduplicate: %+v %+v", s1, s2)
	}
}
