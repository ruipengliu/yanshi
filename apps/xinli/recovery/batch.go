package recovery

import (
	"errors"
	"fmt"
)

func ValidateBatch(line string, b Batch) error {
	if line == "" {
		return errors.New("recovery: missing business line")
	}
	if cp := b.Checkpoint; cp != nil && (cp.SessionID == "" || cp.EndUser == "" || cp.Gone) {
		return errors.New("recovery: invalid checkpoint")
	}
	identities := map[string]string{}
	for _, e := range b.Envelopes {
		if e.EnvelopeID != "" {
			key := fmt.Sprintf("%d:%s:%d", len(e.SessionID), e.SessionID, e.Seq)
			if old, ok := identities[e.EnvelopeID]; ok && old != key {
				return errors.New("recovery: envelope identity conflict")
			}
			identities[e.EnvelopeID] = key
		}
		if e.BusinessLine != line || e.SessionID == "" {
			return errors.New("recovery: invalid envelope scope")
		}
		switch e.Kind {
		case "session_deleted":
			if e.Seq != 0 || e.EndUser != "" || e.EnvelopeID == "" {
				return errors.New("recovery: invalid deletion envelope")
			}
		case "run_completed", "run_failed", "question", "approval_requested", "background_settled", "session_closed":
			if e.Seq == 0 || e.EndUser == "" {
				return errors.New("recovery: invalid event envelope")
			}
		default:
			return errors.New("recovery: unknown envelope kind")
		}
	}
	return nil
}
