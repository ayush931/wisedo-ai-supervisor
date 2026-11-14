package main

// The WIS-626 chat passage probe (manifest "kind": "chat-passages"). In a disposable database it
// seeds one site with valid approved-claim shapes (claims alternate between small_claim_spans and
// large_claim_spans, each within ClaimInput.Validate's 20-span limit; at the 10,000-span bound every
// claim cites 20 spans and nothing is selectable, which the run asserts, while
// chat-passages-load-mixed.json keeps a selectable size) and a foreign tenant, then, as the
// restricted runtime role, times chat passage selection for
// each question twice through the same product code (chat.SelectPassagesUsing): once with the
// WIS-620 on-the-fly ranking (legacyRankSQL, "before") and once with the indexed one
// (chat.RankSQL, "after"). Every question must select identical passages both ways, and no span
// of the foreign tenant may appear in either ranking query's rows. Thresholds are frozen in the manifest before measuring.
//
// Each claim has its own subject entity, with a display name and aliases drawn from the same
// vocabulary as the passages, so the WIS-646 name-matching path (chat.ClaimTermsSQL over every
// claim's terms) runs in every timed selection, at the full claim count (WIS-700). Its own latency
// and plan are reported, and a run where no question matches any claim's name fails as vacuous.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
)

// legacyRankSQL is the WIS-620 ranking query, kept here only as the "before" measurement: it
// re-reads every cited revision body and runs to_tsvector on each span per question. A variable
// only so the integration test can break its scope and prove the probe notices.
var legacyRankSQL = `WITH q AS (SELECT tsvector_to_array(to_tsvector('english',$4)) AS words)
SELECT s.id::text,s.source_id::text,s.revision,octet_length(p.text),
 array_to_string(ARRAY(SELECT w FROM unnest(tsvector_to_array(to_tsvector('english',p.text))) w WHERE w=ANY(q.words)),' ')
FROM supervisor.evidence_spans s
JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision)
CROSS JOIN q
CROSS JOIN LATERAL (SELECT substring(r.body FROM s.start_rune+1 FOR s.end_rune-s.start_rune) AS text) p
WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.id=ANY($3::uuid[])`

type PassageManifest struct {
	SchemaVersion      int      `json:"schema_version"`
	Kind               string   `json:"kind"`
	Issue              string   `json:"issue"`
	Description        string   `json:"description"`
	Seed               int64    `json:"seed"`
	Claims             int      `json:"claims"`
	SmallClaimSpans    int      `json:"small_claim_spans"`
	LargeClaimSpans    int      `json:"large_claim_spans"`
	ForeignTenantSpans int      `json:"foreign_tenant_spans"`
	Queries            int      `json:"queries"`
	Questions          []string `json:"questions"`
	// SelectionCompanion names (relative to this manifest) the manifest a non-selecting bound
	// fixture runs after itself, so its non-empty selection is measured by the same command.
	SelectionCompanion string `json:"selection_companion,omitempty"`
	Thresholds         struct {
		IndexedP95Ms           float64 `json:"indexed_p95_ms"`
		SelectionMismatchesMax int     `json:"selection_mismatches_max"`
		CrossTenantRankedMax   int     `json:"cross_tenant_ranked_max"`
		ErrorsMax              int     `json:"errors_max"`
	} `json:"thresholds"`
	ReferenceHost struct {
		Description               string `json:"description"`
		GOMAXPROCS                int    `json:"gomaxprocs"`
		PostgresLimitsAttestation string `json:"postgres_limits_attestation"`
	} `json:"reference_host"`
}

func (m PassageManifest) validate() error {
	switch {
	case m.SchemaVersion != 1 || m.Kind != "chat-passages" || m.Issue == "":
		return errors.New("schema_version 1, kind chat-passages and issue are required")
	case m.Claims < 2 || m.SmallClaimSpans < 1 || m.LargeClaimSpans < m.SmallClaimSpans || m.spans() > 10000:
		return errors.New("claims >= 2, small_claim_spans >= 1, large_claim_spans >= small, at most 10000 spans")
	case m.ForeignTenantSpans < 1 || m.Queries < 1 || len(m.Questions) == 0:
		return errors.New("foreign_tenant_spans, queries and questions are required")
	case m.Thresholds.IndexedP95Ms <= 0 || m.ReferenceHost.GOMAXPROCS < 1:
		return errors.New("thresholds.indexed_p95_ms and reference_host.gomaxprocs are required")
	}
	// Every claim shape the fixture seeds must be one an approved claim can have (WIS-626 review:
	// a 38-span claim is not, ClaimInput.Validate allows at most 20).
	for _, n := range []int{m.SmallClaimSpans, m.LargeClaimSpans, m.ForeignTenantSpans} {
		if _, err := fixtureClaim(uuid.NewString(), fixtureSpanIDs(n)); err != nil {
			return fmt.Errorf("a claim citing %d spans is not a valid claim: %w", n, err)
		}
	}
	return nil
}

// selects reports whether the fixture has a claim that fits the passage limit. At the 10,000-span
// bound every claim cites 20 spans, so nothing can be selected there; that is asserted, not assumed.
func (m PassageManifest) selects() bool { return m.SmallClaimSpans <= agent.MaxRunPassages }

// fixtureClaim builds a full approved-claim shape citing spans and runs it through the public
// claim contract, so the probe never measures a claim the product could not hold.
func fixtureClaim(id string, spans []string) (knowledge.SnapshotClaim, error) {
	in := knowledge.ClaimInput{ClaimID: id, SubjectID: "00000000-0000-4000-8000-000000000001", Predicate: "objective",
		Literal: &knowledge.Literal{Type: "string", Value: json.RawMessage(`"Load fixture objective"`)}, EffectiveFrom: validFrom, EvidenceClass: "report", SourceSpanIDs: spans}
	if err := in.Validate(); err != nil {
		return knowledge.SnapshotClaim{}, err
	}
	return knowledge.SnapshotClaim{ClaimInput: in, Revision: 1}, nil
}

func fixtureSpanIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = uuid.NewString()
	}
	return out
}

// spans is the fixture's span count: claims alternate small, large, small, ...
func (m PassageManifest) spans() int {
	return (m.Claims+1)/2*m.SmallClaimSpans + m.Claims/2*m.LargeClaimSpans
}

func loadPassageManifest(path string) (PassageManifest, string, error) {
	return loadPassageManifestAs(path, false)
}

// loadPassageManifestAs loads a manifest; a companion may not name a companion of its own.
func loadPassageManifestAs(path string, companion bool) (PassageManifest, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PassageManifest{}, "", err
	}
	var m PassageManifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return PassageManifest{}, "", err
	}
	if err = m.validate(); err != nil {
		return PassageManifest{}, "", err
	}
	switch {
	case companion && m.SelectionCompanion != "":
		return PassageManifest{}, "", errors.New("a selection_companion may not name its own")
	case m.selects() && m.SelectionCompanion != "":
		return PassageManifest{}, "", errors.New("selection_companion is only for a fixture that cannot select")
	case !m.selects():
		// A fixture where nothing fits the passage limit compares empty selections; it must bring a
		// companion that selects, with the same frozen thresholds and reference host.
		if m.SelectionCompanion == "" {
			return PassageManifest{}, "", errors.New("no claim fits the passage limit: selection_companion is required")
		}
		c, _, err := loadPassageManifestAs(filepath.Join(filepath.Dir(path), m.SelectionCompanion), true)
		if err != nil {
			return PassageManifest{}, "", fmt.Errorf("selection_companion: %w", err)
		}
		if !c.selects() || c.Thresholds != m.Thresholds || c.ReferenceHost != m.ReferenceHost {
			return PassageManifest{}, "", errors.New("selection_companion must select, with identical thresholds and reference_host")
		}
	}
	sum := sha256.Sum256(raw)
	return m, hex.EncodeToString(sum[:]), nil
}

type PassageReport struct {
	SchemaVersion       int             `json:"schema_version"`
	Issue               string          `json:"issue"`
	Result              string          `json:"result"`
	Command             string          `json:"command"`
	GitSHA              string          `json:"git_sha"`
	GitDirty            bool            `json:"git_dirty"`
	StartedAt           string          `json:"started_at"`
	ManifestPath        string          `json:"manifest_path"`
	ManifestSHA256      string          `json:"manifest_sha256"`
	Manifest            PassageManifest `json:"manifest"`
	Host                map[string]any  `json:"host"`
	Spans               int             `json:"spans"`
	SeedSeconds         float64         `json:"seed_seconds"`
	Before              Latency         `json:"before_wis620_on_the_fly"`
	After               Latency         `json:"after_wis626_indexed"`
	SelectionMismatches int             `json:"selection_mismatches"`
	// RankRowMismatches counts questions whose two ranking queries returned different rows (span,
	// source, revision, size, matched words): equivalence is checked even where nothing is selected.
	RankRowMismatches int `json:"rank_row_mismatches"`
	CrossTenantRanked int `json:"cross_tenant_ranked"`
	Errors            int `json:"errors"`
	PassagesSelected  int `json:"passages_selected_total"`
	// RankPlan is EXPLAIN (ANALYZE, BUFFERS) of chat.RankSQL for the first question, as the
	// runtime role on this fixture: which index the planner actually uses is recorded, not assumed.
	RankPlan []string `json:"rank_plan"`
	// ClaimTerms times chat.ClaimTermsSQL alone over every claim's terms (WIS-700), once per query;
	// it is also inside every "after" selection. ClaimTermsPlan is its EXPLAIN (ANALYZE, BUFFERS)
	// for the first question. NameMatches counts, over the questions, the claims whose subject's
	// display name or aliases alone hold a question word (predicate words are not counted).
	// NameChangedSelections counts the questions whose timed "after" selection differs from the
	// same selection made without the entities: names did change what chat would read.
	ClaimTerms            Latency  `json:"claim_terms"`
	ClaimTermsPlan        []string `json:"claim_terms_plan"`
	NameMatches           int      `json:"name_matches"`
	NameChangedSelections int      `json:"name_changed_selections"`
	Failures              []string `json:"failures"`
}

func runPassages(manifestPath, outDir string, getenv func(string) string, stdout, stderr io.Writer) int {
	blocked := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "BLOCKED: "+format+"\n", a...)
		return 2
	}
	m, sha, err := loadPassageManifest(manifestPath)
	if err != nil {
		return blocked("passage manifest %s: %v", manifestPath, err)
	}
	adminURL, runtimeURL := getenv("SUPERVISOR_TEST_ADMIN_URL"), getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		return blocked("SUPERVISOR_TEST_ADMIN_URL and SUPERVISOR_TEST_DATABASE_URL are required (real PostgreSQL)")
	}
	r := PassageReport{SchemaVersion: 1, Issue: m.Issue, StartedAt: time.Now().UTC().Format(time.RFC3339), ManifestPath: manifestPath, ManifestSHA256: sha, Manifest: m, Failures: []string{}}
	r.Command = getenv("SUPERVISOR_LOAD_COMMAND")
	if r.Command == "" {
		r.Command = strings.Join(os.Args, " ")
	}
	r.GitSHA = gitOutput("rev-parse", "HEAD")
	r.GitDirty = gitOutput("status", "--porcelain", "--untracked-files=no") != ""
	limits := getenv("SUPERVISOR_LOAD_PG_LIMITS")
	reference := runtime.GOMAXPROCS(0) == m.ReferenceHost.GOMAXPROCS && limits == m.ReferenceHost.PostgresLimitsAttestation
	r.Host = map[string]any{"num_cpu": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "mem_total": memTotal(), "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"postgres_limits": limits, "evidence": map[bool]string{true: "operator-attested reference-host configuration", false: "not reference-host evidence"}[reference]}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err = measurePassages(ctx, m, adminURL, runtimeURL, &r); err != nil {
		var block blockedError
		if errors.As(err, &block) {
			return blocked("%v", err)
		}
		r.Failures = append(r.Failures, "run aborted: "+err.Error())
	}
	if r.After.P95Ms > m.Thresholds.IndexedP95Ms {
		r.Failures = append(r.Failures, fmt.Sprintf("indexed p95 %.3f ms exceeds %.0f", r.After.P95Ms, m.Thresholds.IndexedP95Ms))
	}
	if r.SelectionMismatches > m.Thresholds.SelectionMismatchesMax {
		r.Failures = append(r.Failures, fmt.Sprintf("%d questions selected different passages before and after", r.SelectionMismatches))
	}
	if r.RankRowMismatches > m.Thresholds.SelectionMismatchesMax {
		r.Failures = append(r.Failures, fmt.Sprintf("%d questions ranked different rows before and after", r.RankRowMismatches))
	}
	if r.CrossTenantRanked > m.Thresholds.CrossTenantRankedMax {
		r.Failures = append(r.Failures, fmt.Sprintf("%d foreign-tenant spans ranked", r.CrossTenantRanked))
	}
	if r.NameMatches == 0 {
		r.Failures = append(r.Failures, "no question matched any claim's name: the name-matching path was not exercised")
	}
	switch {
	case m.selects() && r.PassagesSelected == 0:
		r.Failures = append(r.Failures, "no question selected any passage: the comparison would be vacuous")
	case !m.selects() && r.PassagesSelected != 0:
		r.Failures = append(r.Failures, fmt.Sprintf("%d passages selected although no claim fits the passage limit", r.PassagesSelected))
	}
	if r.Errors > m.Thresholds.ErrorsMax {
		r.Failures = append(r.Failures, fmt.Sprintf("%d errors", r.Errors))
	}
	var exit int
	r.Result, exit = verdict(len(r.Failures), reference)
	if err = os.MkdirAll(outDir, 0o755); err != nil {
		return blocked("report directory: %v", err)
	}
	// Named by manifest as well as commit: the bound and mixed manifests run at the same SHA.
	path := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(manifestPath), ".json")+"-"+r.GitSHA+".json")
	body, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return blocked("write report: %v", err)
	}
	fmt.Fprintf(stdout, "%s chat passages (%s)\n", r.Result, r.Host["evidence"])
	fmt.Fprintf(stdout, "fixture: %d claims (%d and %d spans alternating) = %d spans, %d foreign-tenant spans; seeded in %.1f s\n", m.Claims, m.SmallClaimSpans, m.LargeClaimSpans, r.Spans, m.ForeignTenantSpans, r.SeedSeconds)
	fmt.Fprintf(stdout, "before (WIS-620 on the fly): %d questions, p50 %.3f p95 %.3f p99 %.3f max %.3f ms\n", r.Before.Count, r.Before.P50Ms, r.Before.P95Ms, r.Before.P99Ms, r.Before.MaxMs)
	fmt.Fprintf(stdout, "after (WIS-626 indexed): %d questions, p50 %.3f p95 %.3f p99 %.3f max %.3f ms (p95 <= %.0f)\n", r.After.Count, r.After.P50Ms, r.After.P95Ms, r.After.P99Ms, r.After.MaxMs, m.Thresholds.IndexedP95Ms)
	fmt.Fprintf(stdout, "claim terms (WIS-646 name matching, %d name matches, %d selections changed by names): %d questions, p50 %.3f p95 %.3f p99 %.3f max %.3f ms\n", r.NameMatches, r.NameChangedSelections, r.ClaimTerms.Count, r.ClaimTerms.P50Ms, r.ClaimTerms.P95Ms, r.ClaimTerms.P99Ms, r.ClaimTerms.MaxMs)
	fmt.Fprintf(stdout, "passages selected %d, selection mismatches %d, rank row mismatches %d, foreign spans ranked %d, errors %d; report %s\n", r.PassagesSelected, r.SelectionMismatches, r.RankRowMismatches, r.CrossTenantRanked, r.Errors, path)
	for _, f := range r.Failures {
		fmt.Fprintln(stderr, "FAIL:", f)
	}
	if r.Result == "BLOCKED" {
		fmt.Fprintln(stderr, "BLOCKED: every threshold was met, but not on the recorded reference host; this run is not qualification evidence")
	}
	if m.SelectionCompanion != "" {
		companion := filepath.Join(filepath.Dir(manifestPath), m.SelectionCompanion)
		fmt.Fprintf(stdout, "selection companion %s:\n", companion)
		c := runPassages(companion, outDir, getenv, stdout, stderr)
		if exit != 1 && (c == 1 || c > exit) { // FAIL outranks BLOCKED outranks PASS
			exit = c
		}
	}
	return exit
}

// vocabulary makes passage text with overlapping words, so questions match some spans strongly,
// many weakly and most not at all.
var vocabulary = []string{"oven", "curing", "temperature", "degrees", "conveyor", "press", "stamping", "weld", "inspection", "sample",
	"supplier", "steel", "coil", "storage", "dispatch", "pallet", "torque", "bolt", "paint", "humidity", "shift", "operator", "setup", "cycle", "batch"}

func measurePassages(ctx context.Context, m PassageManifest, adminURL, runtimeURL string, r *PassageReport) error {
	control, err := sql.Open("pgx", adminURL)
	if err != nil {
		return blockedError{fmt.Errorf("admin database: %w", err)}
	}
	defer control.Close()
	if err = control.PingContext(ctx); err != nil {
		return blockedError{errors.New("admin database unreachable")}
	}
	database := "chat_passage_load_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = control.ExecContext(ctx, `CREATE DATABASE `+database); err != nil {
		return blockedError{fmt.Errorf("create disposable database: %w", err)}
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, _ = control.ExecContext(cleanup, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
	}()
	connect := func(raw string) (*sql.DB, error) {
		u, e := url.Parse(raw)
		if e != nil {
			return nil, e
		}
		u.Path = "/" + database
		return sql.Open("pgx", u.String())
	}
	admin, err := connect(adminURL)
	if err != nil {
		return blockedError{err}
	}
	defer admin.Close()
	rt, err := connect(runtimeURL)
	if err != nil {
		return blockedError{err}
	}
	defer rt.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		return blockedError{fmt.Errorf("migrate disposable database: %w", err)}
	}
	rnd := rand.New(rand.NewSource(m.Seed))
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = uuid.Must(uuid.NewRandomFromReader(rnd)).String()
		}
		return out
	}
	started := time.Now()
	tenant, site, foreignTenant, foreignSite := ids(1)[0], ids(1)[0], ids(1)[0], ids(1)[0]
	// seedSource writes one source revision of n sentences and one span per sentence, as the
	// superuser inside the tenant's scope; the 073 trigger writes each span's search row.
	seedSource := func(tenantID, siteID string, n int) ([]string, error) {
		source, spanIDs := ids(1)[0], ids(n)
		var sentences []string
		var starts, ends []int
		pos := 0
		for range n {
			words := make([]string, 8)
			for w := range words {
				words[w] = vocabulary[rnd.Intn(len(vocabulary))]
			}
			s := strings.Join(words, " ") + "."
			if len(sentences) > 0 {
				pos++
			}
			starts, ends = append(starts, pos), append(ends, pos+len(s))
			pos += len(s)
			sentences = append(sentences, s)
		}
		body := strings.Join(sentences, " ")
		sum := sha256.Sum256([]byte(body))
		tx, e := admin.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		for _, step := range []struct {
			q    string
			args []any
		}{
			{`SELECT set_config('supervisor.tenant_id',$1,true)`, []any{tenantID}},
			{`INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`, []any{tenantID, siteID, source}},
			{`INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,1,'document','Load SOP',$4,$5,$6,$6,'load','load')`,
				[]any{tenantID, siteID, source, body, hex.EncodeToString(sum[:]), len(body)}},
			{`INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject)
 SELECT $1,$2,u.id,$3,1,u.s,u.e,'load','load' FROM unnest($4::uuid[],$5::int[],$6::int[]) AS u(id,s,e)`, []any{tenantID, siteID, source, spanIDs, starts, ends}},
		} {
			if _, e = tx.ExecContext(ctx, step.q, step.args...); e != nil {
				return nil, e
			}
		}
		return spanIDs, tx.Commit()
	}
	for _, s := range []struct{ tenant, site, name string }{{tenant, site, "chat passage load"}, {foreignTenant, foreignSite, "foreign tenant"}} {
		if _, err = admin.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,$2)`, s.tenant, s.name); err == nil {
			_, err = admin.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site')`, s.tenant, s.site)
		}
		if err != nil {
			return fmt.Errorf("seed tenant: %w", err)
		}
	}
	claimIDs := ids(m.Claims)
	slices.Sort(claimIDs)
	claims := make([]knowledge.SnapshotClaim, 0, m.Claims+1)
	entities := make([]knowledge.SnapshotEntity, 0, m.Claims)
	for i, id := range claimIDs {
		n := m.SmallClaimSpans
		if i%2 == 1 {
			n = m.LargeClaimSpans
		}
		spans, e := seedSource(tenant, site, n)
		if e != nil {
			return fmt.Errorf("seed claim source: %w", e)
		}
		slices.Sort(spans)
		c, e := fixtureClaim(id, spans)
		if e != nil {
			return fmt.Errorf("fixture claim %s: %w", id, e)
		}
		ent := fixtureEntity(i)
		c.SubjectID = ent.EntityID
		claims, entities = append(claims, c), append(entities, ent)
		r.Spans += len(spans)
	}
	foreign, err := seedSource(foreignTenant, foreignSite, m.ForeignTenantSpans)
	if err != nil {
		return fmt.Errorf("seed foreign tenant: %w", err)
	}
	// A claim citing every foreign span: under forced RLS it can never be ranked or selected.
	foreignClaim, err := fixtureClaim("00000000-0000-4000-8000-000000000000", foreign)
	if err != nil {
		return fmt.Errorf("foreign fixture claim: %w", err)
	}
	claims = append(claims, foreignClaim)
	r.SeedSeconds = time.Since(started).Seconds()
	isForeign := map[string]bool{}
	for _, id := range foreign {
		isForeign[id] = true
	}

	sc := chat.Scope{TenantID: tenant, SiteID: site}
	pick := func(question, rankSQL string, entities []knowledge.SnapshotEntity) ([]agent.RunPassage, time.Duration, error) {
		tx, e := rt.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if e != nil {
			return nil, 0, e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); e != nil {
			return nil, 0, e
		}
		t0 := time.Now()
		out, e := chat.SelectPassagesUsing(ctx, tx, sc, claims, entities, question, rankSQL)
		return out, time.Since(t0), e
	}
	texts := chat.ClaimTermTexts(claims, entities)
	if err = explainRank(ctx, rt, tenant, sc, claims, m.Questions[0], r); err != nil {
		return fmt.Errorf("explain rank query: %w", err)
	}
	if r.ClaimTermsPlan, err = explainQuery(ctx, rt, tenant, chat.ClaimTermsSQL, m.Questions[0], texts); err != nil {
		return fmt.Errorf("explain claim terms query: %w", err)
	}
	// Names and aliases alone, per claim, so a predicate word never counts as a name match.
	subjectNames := map[string]string{}
	for _, e := range entities {
		subjectNames[e.EntityID] = strings.Join(append([]string{e.Name}, e.Aliases...), " ")
	}
	nameTexts := make([]string, len(claims))
	for i, c := range claims {
		nameTexts[i] = subjectNames[c.SubjectID]
	}
	for _, question := range m.Questions {
		matched, _, e := claimTermMatches(ctx, rt, tenant, question, nameTexts)
		if e != nil {
			return fmt.Errorf("claim terms check: %w", e)
		}
		r.NameMatches += matched
	}
	// Foreign spans are counted in what each ranking query returns, before selection: the
	// whole-claim passage budget would otherwise hide a ranked foreign span (WIS-626 review).
	// Untimed, once per question.
	var allIDs []string
	for _, c := range claims {
		allIDs = append(allIDs, c.SourceSpanIDs...)
	}
	// The two queries' rows are compared too, so equivalence holds even where nothing is selected.
	for _, question := range m.Questions {
		var sets [2][]rankRow
		for i, q := range []string{legacyRankSQL, chat.RankSQL} {
			rows, e := rankRows(ctx, rt, tenant, sc, allIDs, question, q)
			if e != nil {
				return fmt.Errorf("rank rows check: %w", e)
			}
			for _, row := range rows {
				if isForeign[row.id] {
					r.CrossTenantRanked++
				}
			}
			sets[i] = rows
		}
		if !slices.Equal(sets[0], sets[1]) {
			r.RankRowMismatches++
		}
	}
	var before, after, terms []time.Duration
	timed := map[string][]agent.RunPassage{} // each question's first timed "after" selection
	for i := range m.Queries {
		question := m.Questions[i%len(m.Questions)]
		if _, d, e := claimTermMatches(ctx, rt, tenant, question, texts); e != nil {
			r.Errors++
		} else {
			terms = append(terms, d)
		}
		// Alternate which variant runs first, so a warm cache favours neither.
		order := []string{legacyRankSQL, chat.RankSQL}
		if i%2 == 1 {
			order[0], order[1] = order[1], order[0]
		}
		got := map[string][]agent.RunPassage{}
		for _, q := range order {
			// Both variants get the same entities, so they score names alike.
			out, d, e := pick(question, q, entities)
			if e != nil {
				r.Errors++
				continue
			}
			got[q] = out
			if q == chat.RankSQL {
				after = append(after, d)
				if _, seen := timed[question]; !seen {
					timed[question] = out
				}
			} else {
				before = append(before, d)
			}
		}
		if !slices.Equal(got[legacyRankSQL], got[chat.RankSQL]) {
			r.SelectionMismatches++
		}
		r.PassagesSelected += len(got[chat.RankSQL])
	}
	r.Before, r.After, r.ClaimTerms = summarize(before), summarize(after), summarize(terms)
	// Untimed: the same selection without entities. A difference shows the timed path scored names.
	for _, question := range m.Questions {
		out, ok := timed[question]
		if !ok {
			continue
		}
		bare, _, e := pick(question, chat.RankSQL, nil)
		if e != nil {
			r.Errors++
			continue
		}
		if !slices.Equal(out, bare) {
			r.NameChangedSelections++
		}
	}
	return nil
}

type rankRow struct {
	id, source     string
	revision, size int64
	words          string // matched question words, sorted
}

// rankRows runs one ranking query exactly as chat does (runtime role, tenant scope) and returns
// its rows ordered by span ID.
func rankRows(ctx context.Context, rt *sql.DB, tenant string, sc chat.Scope, ids []string, question, rankSQL string) ([]rankRow, error) {
	tx, err := rt.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, rankSQL, sc.TenantID, sc.SiteID, ids, question)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rankRow
	for rows.Next() {
		var row rankRow
		if err = rows.Scan(&row.id, &row.source, &row.revision, &row.size, &row.words); err != nil {
			return nil, err
		}
		words := strings.Fields(row.words)
		slices.Sort(words)
		row.words = strings.Join(words, " ")
		out = append(out, row)
	}
	slices.SortFunc(out, func(x, y rankRow) int { return strings.Compare(x.id, y.id) })
	return out, rows.Err()
}

// fixtureEntity is claim i's subject: a display name and two aliases from the passage vocabulary,
// derived from i alone (never from the seeded stream, so the passages a seed makes are unchanged).
func fixtureEntity(i int) knowledge.SnapshotEntity {
	v := func(k int) string { return vocabulary[k%len(vocabulary)] }
	var e knowledge.SnapshotEntity
	e.EntityID = fmt.Sprintf("00000000-0000-4000-9000-%012d", i+1)
	e.Name = fmt.Sprintf("%s %s cell %d", v(i), v(i/len(vocabulary)+7), i+1)
	e.Aliases = []string{v(i*7+3) + " station", "line " + v(i*11+5)}
	return e
}

// claimTermMatches runs chat.ClaimTermsSQL exactly as chat does (runtime role, tenant scope) and
// returns how many claim texts hold a question word, and how long the query took.
func claimTermMatches(ctx context.Context, rt *sql.DB, tenant, question string, texts []string) (int, time.Duration, error) {
	tx, err := rt.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return 0, 0, err
	}
	t0 := time.Now()
	rows, err := tx.QueryContext(ctx, chat.ClaimTermsSQL, question, texts)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	matched := 0
	for rows.Next() {
		var i int
		var words string
		if err = rows.Scan(&i, &words); err != nil {
			return 0, 0, err
		}
		if words != "" {
			matched++
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, err
	}
	return matched, time.Since(t0), nil
}

// explainQuery returns EXPLAIN (ANALYZE, BUFFERS) of query with args, as the runtime role in the
// tenant's scope, each line cut at 300 bytes.
func explainQuery(ctx context.Context, rt *sql.DB, tenant, query string, args ...any) ([]string, error) {
	tx, err := rt.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			return nil, err
		}
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

// explainRank records the plan of the indexed ranking query for one question, run exactly as
// chat runs it (runtime role, tenant scope, every cited span ID).
func explainRank(ctx context.Context, rt *sql.DB, tenant string, sc chat.Scope, claims []knowledge.SnapshotClaim, question string, r *PassageReport) error {
	var ids []string
	for _, c := range claims {
		ids = append(ids, c.SourceSpanIDs...)
	}
	// The span ID list repeats every cited ID; the plan's shape is what matters.
	plan, err := explainQuery(ctx, rt, tenant, chat.RankSQL, sc.TenantID, sc.SiteID, ids, question)
	r.RankPlan = plan
	return err
}
