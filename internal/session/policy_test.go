package session

import (
	"testing"
	"time"
)

// WIS-290: 30-minute idle, 8-hour absolute and a 15-minute recent-authentication window are the
// conservative defaults, and an unset policy enforces them.
func TestDefaultPolicy(t *testing.T) {
	d := Default()
	if d.Idle != 30*time.Minute || d.Absolute != 8*time.Hour || d.RecentAuth != 15*time.Minute {
		t.Fatalf("defaults: %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if (Policy{}).OrDefault() != d {
		t.Fatal("zero policy did not fall back to the defaults")
	}
	custom := Policy{Idle: 10 * time.Minute, Absolute: time.Hour, RecentAuth: 5 * time.Minute}
	if custom.OrDefault() != custom {
		t.Fatal("a configured policy was replaced")
	}
}

func TestValidateRejectsUnenforceablePolicies(t *testing.T) {
	for name, p := range map[string]Policy{
		"zero idle":           {Absolute: time.Hour, RecentAuth: time.Minute},
		"fractional seconds":  {Idle: 1500 * time.Millisecond, Absolute: time.Hour, RecentAuth: time.Minute},
		"idle over absolute":  {Idle: 20 * time.Minute, Absolute: 10 * time.Minute, RecentAuth: time.Minute},
		"proof over absolute": {Idle: time.Minute, Absolute: 10 * time.Minute, RecentAuth: 15 * time.Minute},
		"looser idle":         {Idle: 31 * time.Minute, Absolute: time.Hour, RecentAuth: time.Minute},
		"looser absolute":     {Idle: time.Minute, Absolute: 9 * time.Hour, RecentAuth: time.Minute},
		"looser proof age":    {Idle: 30 * time.Minute, Absolute: time.Hour, RecentAuth: 16 * time.Minute},
	} {
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestActiveAndRecent(t *testing.T) {
	p := Default()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if !p.Active(now.Add(time.Hour), now.Add(-29*time.Minute), now) {
		t.Fatal("a session seen 29 minutes ago was rejected")
	}
	if p.Active(now.Add(time.Hour), now.Add(-30*time.Minute), now) {
		t.Fatal("a session idle for exactly the limit was accepted")
	}
	if p.Active(now, now, now) {
		t.Fatal("an expired session was accepted")
	}
	if !p.Recent(now.Add(-15*time.Minute), now) || p.Recent(now.Add(-16*time.Minute), now) {
		t.Fatal("recent-authentication window boundary")
	}
	if p.Recent(now.Add(time.Second), now) || p.Recent(time.Time{}, now) {
		t.Fatal("a future or missing proof counted as recent")
	}
}
