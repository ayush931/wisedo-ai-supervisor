// Package session holds the one browser-session policy every Supervisor service applies
// (WIS-290 "Identity and tenant contract"): an idle limit, an absolute lifetime and the maximum
// age of the identity proof required for privileged decisions. It replaces limits that were
// previously repeated as literals in each package, so a deployment configures them once.
//
// A Policy is an immutable value passed to each service at construction; there is no global
// mutable configuration (ADR 0002 Sec. 16).
package session

import (
	"errors"
	"time"
)

// Policy is the browser-session policy.
type Policy struct {
	// Idle is how long a session may go unused before it is no longer accepted.
	Idle time.Duration
	// Absolute is a session's maximum lifetime from login, regardless of activity.
	Absolute time.Duration
	// RecentAuth is the maximum age of the verified OIDC auth_time for privileged actions
	// (approvals, membership changes, reviews).
	RecentAuth time.Duration
}

// Default is the conservative WIS-290 default: 30-minute idle, 8-hour absolute and a
// 15-minute recent-authentication window. These are configurable engineering defaults, not a
// customer's legal policy.
func Default() Policy {
	return Policy{Idle: 30 * time.Minute, Absolute: 8 * time.Hour, RecentAuth: 15 * time.Minute}
}

// Validate rejects a policy that cannot be enforced consistently:
//   - every limit is a positive whole number of seconds (the database compares in seconds);
//   - no limit is looser than Default. The membership guard in migration 004 enforces the
//     default idle and recent-authentication limits inside the database as ceilings the runtime
//     role cannot change, and the approval-decision CHECK in migration 044 does the same for
//     the recent-authentication window, so a deployment may make sessions stricter but never
//     looser (a looser value fails at boot instead of being capped silently, WIS-699);
//   - an idle limit or proof age longer than the absolute lifetime could never apply.
func (p Policy) Validate() error {
	for _, d := range []time.Duration{p.Idle, p.Absolute, p.RecentAuth} {
		if d < time.Second || d%time.Second != 0 {
			return errors.New("session limits must be positive whole seconds")
		}
	}
	d := Default()
	if p.Idle > d.Idle || p.Absolute > d.Absolute || p.RecentAuth > d.RecentAuth {
		return errors.New("session limits may only be stricter than the defaults (30m idle, 8h absolute, 15m recent authentication)")
	}
	if p.Idle > p.Absolute || p.RecentAuth > p.Absolute {
		return errors.New("session idle limit and recent-authentication window cannot exceed the absolute lifetime")
	}
	return nil
}

// OrDefault returns p, or Default when p is the zero value. Services use it so a constructor
// that is not given a policy still enforces the conservative defaults.
func (p Policy) OrDefault() Policy {
	if p == (Policy{}) {
		return Default()
	}
	return p
}

// Active reports whether a session that expires at expiresAt and was last seen at lastSeen is
// still usable at now.
func (p Policy) Active(expiresAt, lastSeen, now time.Time) bool {
	return expiresAt.After(now) && lastSeen.After(now.Add(-p.Idle))
}

// Recent reports whether an identity proof made at authAt is recent enough at now. A proof
// dated in the future is never recent.
func (p Policy) Recent(authAt, now time.Time) bool {
	return !authAt.IsZero() && !authAt.After(now) && !authAt.Before(now.Add(-p.RecentAuth))
}

// IdleSeconds is the idle limit as whole seconds, for SQL predicates such as
// last_seen_at > clock_timestamp() - make_interval(secs => $n).
func (p Policy) IdleSeconds() float64 { return p.Idle.Seconds() }
