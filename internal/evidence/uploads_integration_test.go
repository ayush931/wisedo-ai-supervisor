//go:build integration

package evidence

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// WIS-645 slice 2: QueueMixedUpload stores a mixed PDF as one upload in one transaction, as the
// runtime role: the typed pages as a typed (never model-extracted) document, and the scanned pages
// as jobs linked to the upload. The same key replays everything; another file or page split under
// the key is a conflict that stores nothing; a failure part-way stores nothing; another tenant
// reads none of it.
func TestQueueMixedUploadPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	scope, other := Scope{uuid.NewString(), uuid.NewString()}, Scope{uuid.NewString(), uuid.NewString()}
	owner, outsider := Actor{"https://evidence.example/realm", uuid.NewString(), hash([]byte(uuid.NewString()))}, Actor{"https://evidence.example/realm", uuid.NewString(), hash([]byte(uuid.NewString()))}
	for _, m := range []struct {
		sc Scope
		a  Actor
	}{{scope, owner}, {other, outsider}} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'mixed upload test')`, m.sc.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'mixed upload test')`, m.sc.TenantID, m.sc.SiteID)
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, m.a.SessionHash, m.a.Issuer, m.a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, m.sc.TenantID, m.sc.SiteID, m.a.Issuer, m.a.Subject)
	}
	s := New(runtime)
	// start creates each page's extraction run in the same transaction, as scanRun does.
	start := func(fail int) StartRun {
		calls := 0
		return func(ctx context.Context, tx *sql.Tx, jobID string) (string, error) {
			calls++
			if calls == fail {
				return "", errors.New("run refused")
			}
			id := uuid.NewString()
			_, e := tx.ExecContext(ctx, `INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'extraction',$4,$4,$5,'fixture',false,'vision','r','p','t',1,1,1,1,1,1)`, scope.TenantID, scope.SiteID, id, jobID, strings.Repeat("a", 32))
			return id, e
		}
	}
	page := func(n int, b byte) ScanInput {
		return ScanInput{Title: "Receiving SOP (page " + string(rune('0'+n)) + " of 3)", MediaType: "image/png", Width: 10, Height: 10, Image: []byte{0x89, b}, OriginalSHA256: hash([]byte{b})}
	}
	typedText := "Receiving: weigh every coil.\n"
	input := MixedUploadInput{
		Title: "Receiving SOP", OriginalSHA256: hash([]byte("receiving.pdf")), PageCount: 3,
		Document: DocumentInput{Title: "Receiving SOP", Text: typedText, OriginalSHA256: hash([]byte("receiving.pdf")), Spans: []Range{{Start: 0, End: 28}}},
		Pages:    []ScanInput{page(2, 2), page(3, 3)}, PageNumbers: []int{2, 3},
	}
	uploads := func() int {
		return count(`SELECT count(*) FROM supervisor.document_uploads WHERE tenant_id=$1`, scope.TenantID)
	}
	sources := func() int {
		return count(`SELECT count(*) FROM supervisor.evidence_heads WHERE tenant_id=$1`, scope.TenantID)
	}

	// A failure on the second page stores nothing: no upload, no typed source, no job.
	if _, err = s.QueueMixedUpload(ctx, owner, scope, input, uuid.NewString(), start(2)); err == nil || uploads() != 0 || sources() != 0 ||
		count(`SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1`, scope.TenantID) != 0 {
		t.Fatalf("a failed upload stored something: %v", err)
	}
	// Without a vision model a new mixed upload is ErrScansUnavailable (EXTRACTION_UNAVAILABLE to
	// the caller, as for any scan) and stores nothing, not ErrInvalid.
	if _, err = s.QueueMixedUpload(ctx, owner, scope, input, uuid.NewString(), nil); !errors.Is(err, ErrScansUnavailable) || uploads() != 0 || sources() != 0 {
		t.Fatalf("no vision model: %v, uploads %d, sources %d", err, uploads(), sources())
	}

	key := uuid.NewString()
	got, err := s.QueueMixedUpload(ctx, owner, scope, input, key, start(0))
	if err != nil {
		t.Fatal(err)
	}
	if got.UploadID == nil || got.Document == nil || len(got.Jobs) != 2 || got.Document.Source.Text != typedText || len(got.Document.Spans) != 1 ||
		got.Jobs[0].Title != "Receiving SOP (page 2 of 3)" || got.Jobs[1].Title != "Receiving SOP (page 3 of 3)" {
		t.Fatalf("stored %+v", got)
	}
	if got.Document.Source.Extraction != nil {
		t.Fatalf("typed pages stored as model-extracted text (rule 11): %+v", got.Document.Source.Extraction)
	}
	if n := count(`SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND upload_id=$2`, scope.TenantID, *got.UploadID); n != 2 {
		t.Fatalf("%d jobs linked to the upload", n)
	}
	if n := count(`SELECT count(*) FROM supervisor.document_uploads WHERE tenant_id=$1 AND upload_id=$2 AND typed_source_id=$3 AND page_count=3`, scope.TenantID, *got.UploadID, got.Document.Source.SourceID); n != 1 {
		t.Fatalf("upload row %d", n)
	}
	if n := count(`SELECT count(*) FROM supervisor.evidence_revisions WHERE tenant_id=$1 AND source_id=$2 AND extraction IS NULL`, scope.TenantID, got.Document.Source.SourceID); n != 1 {
		t.Fatalf("typed revision %d", n)
	}

	// The same key and file replays the stored upload: same IDs, nothing new.
	again, err := s.QueueMixedUpload(ctx, owner, scope, input, key, start(0))
	if err != nil || *again.UploadID != *got.UploadID || again.Document.Source.SourceID != got.Document.Source.SourceID ||
		again.Jobs[0].JobID != got.Jobs[0].JobID || again.Jobs[1].JobID != got.Jobs[1].JobID || uploads() != 1 || sources() != 1 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	// The same key with another file, title or page split is a conflict and stores nothing.
	otherFile, otherSplit, otherTitle := input, input, input
	otherFile.OriginalSHA256 = hash([]byte("another.pdf"))
	otherSplit.PageNumbers = []int{1, 3}
	otherTitle.Title = "Renamed"
	for name, in := range map[string]MixedUploadInput{"another file": otherFile, "another page split": otherSplit, "another title": otherTitle} {
		if _, err = s.QueueMixedUpload(ctx, owner, scope, in, key, start(0)); !errors.Is(err, ErrConflict) || uploads() != 1 || sources() != 1 {
			t.Fatalf("%s: %v (%d uploads, %d sources)", name, err, uploads(), sources())
		}
	}
	// A PDF whose every page is scanned is not a mixed upload.
	allScanned := input
	allScanned.PageCount = 2
	if _, err = s.QueueMixedUpload(ctx, owner, scope, allScanned, uuid.NewString(), start(0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no typed page: %v", err)
	}
	// Another tenant's owner reads none of the jobs.
	if _, err = s.ReadScan(ctx, outsider, other, got.Jobs[0].JobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant job read: %v", err)
	}

	// WIS-645 review: one key names one upload, whatever its shape. Both shapes arrive on
	// /source-scan-batches, so a scan-only batch under a mixed upload's key, or a mixed upload under
	// a scan-only batch's key, is a conflict and stores nothing.
	jobs := func() int {
		return count(`SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1`, scope.TenantID)
	}
	before := jobs()
	if _, err = s.QueueScanBatch(ctx, owner, scope, input.Pages, key, start(0)); !errors.Is(err, ErrConflict) || jobs() != before {
		t.Fatalf("a scan batch under a mixed upload's key: %v (%d new jobs)", err, jobs()-before)
	}
	batchKey := uuid.NewString()
	if _, err = s.QueueScanBatch(ctx, owner, scope, []ScanInput{page(1, 7), page(2, 8)}, batchKey, start(0)); err != nil {
		t.Fatal(err)
	}
	before, beforeUploads, beforeSources := jobs(), uploads(), sources()
	if _, err = s.QueueMixedUpload(ctx, owner, scope, input, batchKey, start(0)); !errors.Is(err, ErrConflict) || jobs() != before || uploads() != beforeUploads || sources() != beforeSources {
		t.Fatalf("a mixed upload under a scan batch's key: %v (%d new jobs, %d new uploads, %d new sources)", err, jobs()-before, uploads()-beforeUploads, sources()-beforeSources)
	}
}
