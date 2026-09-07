package policies

import "time"

func rolloutOriginMatchesPolicy(policy Policy, origin time.Time, runs []Run, timestampLayout string) bool {
	if len(runs) == 0 {
		return false
	}
	boundary, valid := policyRecoveryBoundary(policy, timestampLayout)
	if !valid {
		return false
	}
	if !origin.Before(boundary) {
		return true
	}
	// The scheduler admits the whole current minute. A policy created during
	// that minute can legitimately start after its canonical slot; its first
	// persisted row must prove the policy already existed when work started.
	// A subsequent edit must not inherit that earlier configuration's origin.
	if !boundary.Truncate(time.Minute).Equal(origin) {
		return false
	}
	first := time.Time{}
	for _, run := range runs {
		created, known := parsePolicyInstant(run.CreatedAt, timestampLayout)
		if !known {
			return false
		}
		if first.IsZero() || created.Before(first) {
			first = created
		}
	}
	return !first.Before(boundary) && first.Before(origin.Add(time.Minute))
}
