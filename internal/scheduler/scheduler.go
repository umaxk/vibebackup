package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"vbackuper/internal/config"
	"vbackuper/internal/service"
	"vbackuper/internal/store"
)

// Scheduler keeps cron entries in sync with stored projects.
type Scheduler struct {
	cron   *cron.Cron
	parser cron.Parser
	svc    *service.Service
	st     *store.Store
	log    *slog.Logger
	loc    *time.Location

	mu  sync.Mutex
	ids map[int64]cron.EntryID
}

// New creates a scheduler using the standard 5-field cron syntax.
func New(cfg *config.Config, st *store.Store, svc *service.Service, log *slog.Logger) *Scheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &Scheduler{
		cron:   cron.New(cron.WithParser(parser), cron.WithLocation(cfg.Location)),
		parser: parser,
		svc:    svc,
		st:     st,
		log:    log,
		loc:    cfg.Location,
		ids:    map[int64]cron.EntryID{},
	}
}

// Start loads projects, schedules them and performs catch-up runs.
func (s *Scheduler) Start(ctx context.Context) error {
	projects, err := s.st.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if err := s.Sync(project); err != nil {
			s.log.Warn("некорректное расписание проекта", "project", project.Name, "project_id", project.ID, "err", err)
		}
	}
	s.cron.Start()
	s.catchUp(ctx, projects)
	return nil
}

// Stop stops the scheduler; already started jobs continue.
func (s *Scheduler) Stop() { s.cron.Stop() }

// Sync adds, updates or removes the cron entry of a project.
func (s *Scheduler) Sync(project *store.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.ids[project.ID]; ok {
		s.cron.Remove(id)
		delete(s.ids, project.ID)
	}
	if project.Schedule == "" || !project.Enabled {
		return nil
	}

	name := project.Name
	id := project.ID
	entryID, err := s.cron.AddFunc(project.Schedule, func() {
		if _, err := s.svc.StartProjectBackup(context.Background(), id, "schedule"); err != nil {
			if errors.Is(err, service.ErrProjectBusy) {
				s.log.Info("запуск по расписанию пропущен: прогон уже идёт", "project", name)
			} else {
				s.log.Error("не удалось запустить прогон по расписанию", "project", name, "err", err)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("расписание %q: %w", project.Schedule, err)
	}
	s.ids[project.ID] = entryID
	return nil
}

// Remove drops the cron entry of a deleted project.
func (s *Scheduler) Remove(projectID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.ids[projectID]; ok {
		s.cron.Remove(id)
		delete(s.ids, projectID)
	}
}

// NextRun returns the next scheduled run of a project, if any.
func (s *Scheduler) NextRun(projectID int64) *time.Time {
	s.mu.Lock()
	entryID, ok := s.ids[projectID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	entry := s.cron.Entry(entryID)
	if entry.ID == 0 || entry.Next.IsZero() {
		return nil
	}
	next := entry.Next
	return &next
}

// NextRuns returns the next n runs of a cron expression.
func (s *Scheduler) NextRuns(schedule string, n int) ([]time.Time, error) {
	sch, err := s.parser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		now = sch.Next(now)
		out = append(out, now)
	}
	return out, nil
}

// Catch-up: if a scheduled run was missed within the grace window (for example
// because the container was restarted), trigger it once.
func (s *Scheduler) catchUp(ctx context.Context, projects []*store.Project) {
	grace := s.svc.GraceMinutes(ctx)
	if grace <= 0 {
		return
	}
	now := time.Now()
	window := now.Add(-time.Duration(grace) * time.Minute)
	for _, project := range projects {
		if !project.Enabled || project.Schedule == "" {
			continue
		}
		sch, err := s.parser.Parse(project.Schedule)
		if err != nil {
			continue
		}
		prev := sch.Next(window)
		if prev.After(now) {
			continue // nothing scheduled inside the window
		}
		last, err := s.st.LastProjectBackup(ctx, project.ID)
		if err != nil {
			continue
		}
		if last != nil && !last.StartedAt.Before(prev) {
			continue // already covered
		}
		projectID := project.ID
		name := project.Name
		s.log.Info("догоняющий прогон после старта", "project", name, "missed_at", prev.Format(time.RFC3339))
		go func() {
			if _, err := s.svc.StartProjectBackup(context.Background(), projectID, "startup"); err != nil {
				if !errors.Is(err, service.ErrProjectBusy) {
					s.log.Warn("догоняющий прогон не удался", "project", name, "err", err)
				}
			}
		}()
	}
}

// ValidateSchedule reports whether a cron expression is valid.
func (s *Scheduler) ValidateSchedule(schedule string) error {
	if schedule == "" {
		return nil
	}
	_, err := s.parser.Parse(schedule)
	return err
}
