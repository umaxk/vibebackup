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

// Scheduler keeps cron entries in sync with stored targets.
type Scheduler struct {
	cron   *cron.Cron
	parser cron.Parser
	svc    *service.Service
	st     *store.Store
	log    *slog.Logger
	loc    *time.Location

	mu      sync.Mutex
	ids     map[int64]cron.EntryID
	siteIDs map[int64]cron.EntryID
}

// New creates a scheduler using the standard 5-field cron syntax.
func New(cfg *config.Config, st *store.Store, svc *service.Service, log *slog.Logger) *Scheduler {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return &Scheduler{
		cron:    cron.New(cron.WithParser(parser), cron.WithLocation(cfg.Location)),
		parser:  parser,
		svc:     svc,
		st:      st,
		log:     log,
		loc:     cfg.Location,
		ids:     map[int64]cron.EntryID{},
		siteIDs: map[int64]cron.EntryID{},
	}
}

// Start loads targets and sites, schedules them and performs catch-up runs.
func (s *Scheduler) Start(ctx context.Context) error {
	targets, err := s.st.ListTargets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := s.Sync(t); err != nil {
			s.log.Warn("некорректное расписание цели", "target", t.Name, "target_id", t.ID, "err", err)
		}
	}
	sites, err := s.st.ListSites(ctx)
	if err != nil {
		return err
	}
	for _, site := range sites {
		if err := s.SyncSite(site); err != nil {
			s.log.Warn("некорректное расписание сайта", "site", site.Name, "site_id", site.ID, "err", err)
		}
	}
	s.cron.Start()
	s.catchUp(ctx, targets)
	s.catchUpSites(ctx, sites)
	return nil
}

// Stop stops the scheduler; already started jobs continue.
func (s *Scheduler) Stop() { s.cron.Stop() }

// Sync adds, updates or removes the cron entry of a target.
func (s *Scheduler) Sync(t *store.Target) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.ids[t.ID]; ok {
		s.cron.Remove(id)
		delete(s.ids, t.ID)
	}
	if t.Schedule == "" || !t.Enabled {
		return nil
	}

	name := t.Name
	id := t.ID
	entryID, err := s.cron.AddFunc(t.Schedule, func() {
		if _, err := s.svc.StartBackup(context.Background(), id, "schedule"); err != nil {
			if errors.Is(err, service.ErrBusy) {
				s.log.Info("запуск по расписанию пропущен: бэкап уже идёт", "target", name)
			} else {
				s.log.Error("не удалось запустить бэкап по расписанию", "target", name, "err", err)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("расписание %q: %w", t.Schedule, err)
	}
	s.ids[t.ID] = entryID
	return nil
}

// Remove drops the cron entry of a deleted target.
func (s *Scheduler) Remove(targetID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.ids[targetID]; ok {
		s.cron.Remove(id)
		delete(s.ids, targetID)
	}
}

// NextRun returns the next scheduled run of a target, if any.
func (s *Scheduler) NextRun(targetID int64) *time.Time {
	s.mu.Lock()
	entryID, ok := s.ids[targetID]
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

// SyncSite adds, updates or removes the cron entry of a site.
func (s *Scheduler) SyncSite(site *store.Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.siteIDs[site.ID]; ok {
		s.cron.Remove(id)
		delete(s.siteIDs, site.ID)
	}
	if site.Schedule == "" || !site.Enabled {
		return nil
	}

	name := site.Name
	id := site.ID
	entryID, err := s.cron.AddFunc(site.Schedule, func() {
		if _, err := s.svc.StartSiteBackup(context.Background(), id, "schedule"); err != nil {
			if errors.Is(err, service.ErrSiteBusy) {
				s.log.Info("запуск сайта по расписанию пропущен: прогон уже идёт", "site", name)
			} else {
				s.log.Error("не удалось запустить прогон сайта по расписанию", "site", name, "err", err)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("расписание %q: %w", site.Schedule, err)
	}
	s.siteIDs[site.ID] = entryID
	return nil
}

// RemoveSite drops the cron entry of a deleted site.
func (s *Scheduler) RemoveSite(siteID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.siteIDs[siteID]; ok {
		s.cron.Remove(id)
		delete(s.siteIDs, siteID)
	}
}

// NextRunSite returns the next scheduled run of a site, if any.
func (s *Scheduler) NextRunSite(siteID int64) *time.Time {
	s.mu.Lock()
	entryID, ok := s.siteIDs[siteID]
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
func (s *Scheduler) catchUp(ctx context.Context, targets []*store.Target) {
	grace := s.svc.GraceMinutes(ctx)
	if grace <= 0 {
		return
	}
	now := time.Now()
	window := now.Add(-time.Duration(grace) * time.Minute)
	for _, t := range targets {
		if !t.Enabled || t.Schedule == "" {
			continue
		}
		sch, err := s.parser.Parse(t.Schedule)
		if err != nil {
			continue
		}
		prev := sch.Next(window)
		if prev.After(now) {
			continue // nothing scheduled inside the window
		}
		last, err := s.st.LastBackup(ctx, t.ID)
		if err != nil {
			continue
		}
		if last != nil && !last.StartedAt.Before(prev) {
			continue // already covered
		}
		targetID := t.ID
		name := t.Name
		s.log.Info("догоняющий запуск после старта", "target", name, "missed_at", prev.Format(time.RFC3339))
		go func() {
			if _, err := s.svc.StartBackup(context.Background(), targetID, "startup"); err != nil {
				if !errors.Is(err, service.ErrBusy) {
					s.log.Warn("догоняющий запуск не удался", "target", name, "err", err)
				}
			}
		}()
	}
}

// catchUpSites mirrors catchUp for sites.
func (s *Scheduler) catchUpSites(ctx context.Context, sites []*store.Site) {
	grace := s.svc.GraceMinutes(ctx)
	if grace <= 0 {
		return
	}
	now := time.Now()
	window := now.Add(-time.Duration(grace) * time.Minute)
	for _, site := range sites {
		if !site.Enabled || site.Schedule == "" {
			continue
		}
		sch, err := s.parser.Parse(site.Schedule)
		if err != nil {
			continue
		}
		prev := sch.Next(window)
		if prev.After(now) {
			continue
		}
		last, err := s.st.LastSiteBackup(ctx, site.ID)
		if err != nil {
			continue
		}
		if last != nil && !last.StartedAt.Before(prev) {
			continue
		}
		siteID := site.ID
		name := site.Name
		s.log.Info("догоняющий прогон сайта после старта", "site", name, "missed_at", prev.Format(time.RFC3339))
		go func() {
			if _, err := s.svc.StartSiteBackup(context.Background(), siteID, "startup"); err != nil {
				if !errors.Is(err, service.ErrSiteBusy) {
					s.log.Warn("догоняющий прогон сайта не удался", "site", name, "err", err)
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
