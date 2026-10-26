package manager

import (
	"errors"
	"fmt"
	"net/textproto"
	"strings"
	"time"

	"github.com/knadh/listmonk/models"
)

const (
	// quotaExceededToken marks a messenger error as the provider's sending
	// quota being used up. Posthorn replies to such sends with:
	//   451 4.7.0 quota_exceeded retry_after=2026-10-26T00:00:00Z <provider message>
	quotaExceededToken = "quota_exceeded"
	retryAfterPrefix   = "retry_after="

	// deferBuffer is added to the quota reset time before resuming a deferred
	// campaign, to allow for clock skew with the provider.
	deferBuffer = 15 * time.Minute

	// maxDeferrals is the number of consecutive quota deferrals of a campaign,
	// with no successful send in between, after which it is paused instead.
	// This covers the quota being used up again by other senders sharing it
	// before the campaign resumes.
	maxDeferrals = 3
)

// quotaError is a parsed quota_exceeded messenger error.
type quotaError struct {
	// RetryAfter is when the quota resets. Zero if unknown.
	RetryAfter time.Time

	// Reason is the provider's message, if any.
	Reason string
}

// parseQuotaError reports whether err is a quota_exceeded error and parses
// its reset time and message from the SMTP reply text.
func parseQuotaError(err error) (quotaError, bool) {
	if err == nil {
		return quotaError{}, false
	}

	// textproto.Error.Error() quotes the reply text, so read it directly.
	// Otherwise fall back to the error text (eg: a reply wrapped without %w).
	var text string
	if te := (*textproto.Error)(nil); errors.As(err, &te) {
		text = te.Msg
	} else {
		text = strings.ReplaceAll(err.Error(), `"`, "")
	}

	fields := strings.Fields(text)
	for i, f := range fields {
		if f != quotaExceededToken {
			continue
		}

		var (
			q    quotaError
			rest = fields[i+1:]
		)
		if len(rest) > 0 && strings.HasPrefix(rest[0], retryAfterPrefix) {
			if t, err := time.Parse(time.RFC3339, strings.TrimPrefix(rest[0], retryAfterPrefix)); err == nil {
				q.RetryAfter = t
			}
			rest = rest[1:]
		}
		q.Reason = strings.Join(rest, " ")
		return q, true
	}

	return quotaError{}, false
}

// String returns a human readable description for logs and notifications.
func (q quotaError) String() string {
	if q.Reason == "" {
		return "sending quota exceeded"
	}
	return "sending quota exceeded: " + q.Reason
}

// deferCampaign reschedules a campaign that stopped on a used-up quota to
// resume after the quota resets. It returns false if the campaign should be
// paused instead: the reset time is unknown, the campaign has already been
// deferred maxDeferrals times in a row, or rescheduling failed.
func (p *pipe) deferCampaign(q quotaError) bool {
	if q.RetryAfter.IsZero() {
		return false
	}

	// Count consecutive deferrals. A successful send in this run means the
	// last deferral worked, so the count starts over.
	p.m.deferralsMut.Lock()
	n := p.m.deferrals[p.camp.ID] + 1
	if p.sent.Load() > 0 {
		n = 1
	}
	if n > maxDeferrals {
		delete(p.m.deferrals, p.camp.ID)
		p.m.deferralsMut.Unlock()
		p.m.log.Printf("campaign (%s) still over quota after %d deferrals. not deferring again", p.camp.Name, maxDeferrals)
		return false
	}
	p.m.deferrals[p.camp.ID] = n
	p.m.deferralsMut.Unlock()

	sendAt := q.RetryAfter.Add(deferBuffer)
	if err := p.m.store.DeferCampaign(p.camp.ID, sendAt); err != nil {
		p.m.log.Printf("error deferring campaign (%s): %v", p.camp.Name, err)
		return false
	}

	reason := fmt.Sprintf("%s. Resuming at %s (deferral %d of %d)", q, sendAt.Format(time.RFC1123Z), n, maxDeferrals)
	p.m.log.Printf("deferred campaign (%s): %s", p.camp.Name, reason)
	_ = p.m.sendNotif(p.camp, models.CampaignStatusScheduled, reason)

	return true
}

// clearDeferrals forgets a campaign's deferral count once it's done.
func (m *Manager) clearDeferrals(campID int) {
	m.deferralsMut.Lock()
	delete(m.deferrals, campID)
	m.deferralsMut.Unlock()
}
