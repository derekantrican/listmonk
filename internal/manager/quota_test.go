package manager

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/textproto"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
)

func TestParseQuotaError(t *testing.T) {
	reset := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name string
		err  error
		ok   bool
		want quotaError
	}{
		{"nil", nil, false, quotaError{}},
		{"other_error", errors.New("dial tcp: connection refused"), false, quotaError{}},
		{
			"generic_451",
			&textproto.Error{Code: 451, Msg: "4.0.0 Upstream transport failed"},
			false, quotaError{},
		},
		{
			"daily_quota",
			&textproto.Error{Code: 451, Msg: "4.7.0 quota_exceeded retry_after=2026-10-26T00:00:00Z You have exceeded your daily email sending quota."},
			true, quotaError{RetryAfter: reset, Reason: "You have exceeded your daily email sending quota."},
		},
		{
			"wrapped",
			errors.Join(errors.New("send failed"), &textproto.Error{Code: 451, Msg: "4.7.0 quota_exceeded retry_after=2026-10-26T00:00:00Z"}),
			true, quotaError{RetryAfter: reset},
		},
		{
			"wrapped_as_text",
			fmt.Errorf("send failed: %v", &textproto.Error{Code: 451, Msg: "4.7.0 quota_exceeded retry_after=2026-10-26T00:00:00Z daily quota"}),
			true, quotaError{RetryAfter: reset, Reason: "daily quota"},
		},
		{
			"no_reset_time",
			&textproto.Error{Code: 451, Msg: "4.7.0 quota_exceeded You have exceeded your monthly email sending quota."},
			true, quotaError{Reason: "You have exceeded your monthly email sending quota."},
		},
		{
			"bad_reset_time_is_unknown",
			&textproto.Error{Code: 451, Msg: "4.7.0 quota_exceeded retry_after=tomorrow quota"},
			true, quotaError{Reason: "quota"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseQuotaError(tt.err)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !got.RetryAfter.Equal(tt.want.RetryAfter) || got.Reason != tt.want.Reason {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// fakeStore records the campaign state changes made by a pipe's cleanup.
type fakeStore struct {
	Store

	status    string
	sendAt    time.Time
	lastSubID int
	deferErr  error
}

func (s *fakeStore) UpdateCampaignCounts(_ int, _ int, _ int, lastSubID int) error {
	s.lastSubID = lastSubID
	return nil
}

func (s *fakeStore) UpdateCampaignStatus(_ int, status string) error {
	s.status = status
	return nil
}

func (s *fakeStore) DeferCampaign(_ int, sendAt time.Time) error {
	if s.deferErr != nil {
		return s.deferErr
	}
	s.status = models.CampaignStatusScheduled
	s.sendAt = sendAt
	return nil
}

func newTestManager(st Store) *Manager {
	m := New(Config{}, st, nil, log.New(io.Discard, "", 0))
	m.fnNotify = func(string, any) error { return nil }
	return m
}

// quotaRun simulates one pipe run of campaign 1 that stopped on q after
// sending `sent` messages, and runs its cleanup.
func quotaRun(m *Manager, q quotaError, firstID, lastID uint64, sent int64) {
	p := &pipe{camp: &models.Campaign{Base: models.Base{ID: 1}, Name: "test"}, m: m}
	p.firstID.Store(firstID)
	p.lastID.Store(lastID)
	p.sent.Store(sent)
	p.OnQuotaExceeded(q)
	p.cleanup()
}

func TestCleanup_QuotaDefersToResetPlusBuffer(t *testing.T) {
	st := &fakeStore{}
	m := newTestManager(st)
	reset := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

	quotaRun(m, quotaError{RetryAfter: reset}, 10, 42, 5)

	if st.status != models.CampaignStatusScheduled {
		t.Fatalf("status = %q, want scheduled", st.status)
	}
	if want := reset.Add(deferBuffer); !st.sendAt.Equal(want) {
		t.Errorf("sendAt = %v, want %v", st.sendAt, want)
	}
	if st.lastSubID != 42 {
		t.Errorf("checkpoint = %d, want 42 (last sent subscriber)", st.lastSubID)
	}
}

func TestCleanup_QuotaWithoutResetTimePauses(t *testing.T) {
	st := &fakeStore{}
	m := newTestManager(st)

	quotaRun(m, quotaError{Reason: "monthly quota"}, 10, 42, 5)

	if st.status != models.CampaignStatusPaused {
		t.Errorf("status = %q, want paused", st.status)
	}
}

func TestCleanup_QuotaPausesAfterMaxDeferrals(t *testing.T) {
	st := &fakeStore{}
	m := newTestManager(st)
	q := quotaError{RetryAfter: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}

	// The first run sent some messages before hitting the quota; each
	// resumed run after that hits it again without sending anything.
	quotaRun(m, q, 10, 42, 5)
	for i := 1; i < maxDeferrals; i++ {
		quotaRun(m, q, 43, 0, 0)
		if st.status != models.CampaignStatusScheduled {
			t.Fatalf("deferral %d: status = %q, want scheduled", i+1, st.status)
		}
	}

	quotaRun(m, q, 43, 0, 0)
	if st.status != models.CampaignStatusPaused {
		t.Errorf("status = %q, want paused after %d deferrals", st.status, maxDeferrals)
	}
	if _, ok := m.deferrals[1]; ok {
		t.Error("deferral count not cleared after pausing")
	}
}

func TestCleanup_SuccessfulRunResetsDeferrals(t *testing.T) {
	st := &fakeStore{}
	m := newTestManager(st)
	q := quotaError{RetryAfter: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}

	for i := 0; i < maxDeferrals; i++ {
		quotaRun(m, q, 43, 0, 0)
	}
	// This run sent messages before hitting the quota again, so the
	// previous deferral worked and the streak starts over.
	quotaRun(m, q, 43, 60, 10)
	if st.status != models.CampaignStatusScheduled {
		t.Errorf("status = %q, want scheduled", st.status)
	}
	if m.deferrals[1] != 1 {
		t.Errorf("deferrals = %d, want 1", m.deferrals[1])
	}
}

func TestCleanup_DeferFailurePauses(t *testing.T) {
	st := &fakeStore{deferErr: errors.New("db down")}
	m := newTestManager(st)

	quotaRun(m, quotaError{RetryAfter: time.Now().Add(time.Hour)}, 10, 42, 5)

	if st.status != models.CampaignStatusPaused {
		t.Errorf("status = %q, want paused when deferring fails", st.status)
	}
}

func TestCleanup_CheckpointRollsBackWhenNothingSent(t *testing.T) {
	for _, tt := range []struct {
		name            string
		firstID, lastID uint64
		want            int
	}{
		{"sent_some", 10, 42, 42},
		{"sent_none", 10, 0, 9},
		{"sent_none_from_first_subscriber", 1, 0, 0},
		{"fetched_none", 0, 0, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			m := newTestManager(st)
			quotaRun(m, quotaError{}, tt.firstID, tt.lastID, 0)
			if st.lastSubID != tt.want {
				t.Errorf("checkpoint = %d, want %d", st.lastSubID, tt.want)
			}
		})
	}
}
