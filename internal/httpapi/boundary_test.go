package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestIsImportUploadMatchesOnlyTheImportRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/sites/s/operational-sources/x/imports":       true,
		"/v1/sites/s/operational-sources/x/batches":       false,
		"/v1/sites/s/operational-sources/x/imports/extra": false,
		"/v1/sites//operational-sources/x/imports":        false,
		"/v2/sites/s/operational-sources/x/imports":       false,
		"/v1/sites/s/operational-sources/x/imports/":      false,
		"/v1/sites/s/other/x/imports":                     false,
		// WIS-613: a document upload streams under its own 20 MiB limit, not the 1 MiB body limit.
		"/v1/sites/s/source-documents":       true,
		"/v1/sites/s/source-documents/":      false,
		"/v1/sites/s/source-documents/extra": false,
		"/v1/sites//source-documents":        false,
	} {
		if got := isImportUpload(httptest.NewRequest("POST", path, nil)); got != want {
			t.Errorf("POST %s: got %v want %v", path, got, want)
		}
	}
	if isImportUpload(httptest.NewRequest("GET", "/v1/sites/s/operational-sources/x/imports", nil)) {
		t.Error("GET matched")
	}
}
