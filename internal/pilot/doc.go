// Package pilot holds the WIS-631 end-to-end synthetic onboarding pilot. It has no production
// code: pilot_integration_test.go drives the real HTTP API and the worker's own Go entry points
// for two tenants at once against a disposable PostgreSQL, with deterministic scripted model
// output. A fixture run is a contract check, never evidence that the agent works (rule 09).
package pilot
