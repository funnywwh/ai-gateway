package httpapi

import (
	"net/http"
	"testing"
)

// A snapshot's size is what the console shows as 大小 (per row) and 占用 (total_bytes for the
// directory), and it is read from the row — not from memory. v4.7.1 fixed a finishing UPDATE
// that never persisted the size, so a finished, verified 12 GB snapshot listed as "0 B".
//
// The test goes through the wire (POST then GET) because the defect lived exactly in that
// seam: the manager measured a real size, and the list endpoint reported a different number.
func TestAdminRunBackupRecordsTheSize(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	run := f.call(t, http.MethodPost, "/admin/api/v1/backups", `{}`, cookie)
	if run.StatusCode != http.StatusOK {
		run.Body.Close()
		t.Fatalf("run backup status = %d, want 200", run.StatusCode)
	}
	payload := decodeJSONBody(t, run)
	job, _ := payload["job"].(map[string]any)
	if ok, _ := payload["ok"].(bool); !ok || job == nil {
		t.Fatalf("run backup payload = %+v, want ok:true with a job", payload)
	}
	size, _ := job["size_bytes"].(float64)
	if size <= 0 {
		t.Fatalf("job.size_bytes = %v, want the size of the snapshot just taken", job["size_bytes"])
	}

	page := mustPage(t, f, cookie, "/admin/api/v1/backups?limit=5")
	rows := pageRows(t, page)
	if len(rows) != 1 {
		t.Fatalf("the list holds %d rows, want the one backup just taken", len(rows))
	}
	if got, _ := rows[0]["size_bytes"].(float64); got != size {
		t.Fatalf("listed size_bytes = %v, want %v — the row must carry the size the manager measured",
			rows[0]["size_bytes"], size)
	}
	if got, _ := page["total_bytes"].(float64); got != size {
		t.Fatalf("total_bytes = %v, want %v (the console shows it as 占用)", page["total_bytes"], size)
	}
}
