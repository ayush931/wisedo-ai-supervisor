package main

import (
	"context"
	"database/sql"
	"errors"
)

// pilotTenant is one of the two synthetic tenants the WIS-631 browser pilot
// (web/tests/onboarding-pilot.spec.ts) onboards side by side. They are separate from the WIS-520
// demo tenant so the pilot's site-wide snapshots never pick up another spec's approved claims,
// and each pilot user is a member of exactly one pilot site: anything that crossed between the
// two would be visible to the other tenant's users.
type pilotTenant struct {
	tenant, site, tenantName, siteName string
	users                              []pilotUser
}

type pilotUser struct {
	username, role, first, last string
}

var pilotTenants = []pilotTenant{
	{tenant: "00000000-0000-4000-8000-000000000631", site: "00000000-0000-4000-8000-000000000632", tenantName: "Pilot Chakan Stampings", siteName: "Chakan Pilot Plant",
		users: []pilotUser{{"chakan-owner", "owner", "Prakash", "Jadhav"}, {"chakan-lead", "supervisor", "Swati", "Pawar"}, {"chakan-approver", "supervisor", "Nitin", "Gaikwad"}}},
	{tenant: "00000000-0000-4000-8000-000000000633", site: "00000000-0000-4000-8000-000000000634", tenantName: "Pilot Pune Pressings", siteName: "Pune Pilot Plant",
		users: []pilotUser{{"pune-owner", "owner", "Aarti", "Bhosale"}, {"pune-lead", "supervisor", "Mahesh", "Shinde"}, {"pune-approver", "supervisor", "Neha", "Kale"}}},
}

// seedPilotTenants creates the pilot tenants, their sites and their users' memberships. It is
// idempotent, writes nothing else (the browser pilot onboards each site itself through the API)
// and refuses a user whose subject is unknown rather than create a membership without one.
func seedPilotTenants(ctx context.Context, control *sql.DB, issuer string, subjects map[string]string) error {
	for _, p := range pilotTenants {
		if _, err := control.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.tenant, p.tenantName); err != nil {
			return errors.New("cannot provision synthetic pilot tenant")
		}
		if _, err := control.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.tenant, p.site, p.siteName); err != nil {
			return errors.New("cannot provision synthetic pilot site")
		}
		for _, u := range p.users {
			subject := subjects[u.username]
			if subject == "" {
				return errors.New("pilot user " + u.username + " has no identity subject")
			}
			if _, err := control.ExecContext(ctx, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
				p.tenant, p.site, issuer, subject, u.role); err != nil {
				return errors.New("cannot provision synthetic pilot membership")
			}
		}
	}
	return nil
}
