package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

func TestHotloadReportRequiresEveryAttestation(t *testing.T) {
	report := map[string]any{"passed": true, "dshVersion": "0.1.2-rc.1", "fakeRequests": 2,
		"sameSession": true, "workerPidUnchanged": true, "startupAnnouncements": 1,
		"credentialReloadEvent": true, "firstKeyObserved": true, "rotatedKeyObserved": true, "cleaned": true}
	data, _ := json.Marshal(report)
	if err := validateHotloadReport(data); err != nil {
		t.Fatal(err)
	}
	for name, value := range report {
		delete(report, name)
		data, _ := json.Marshal(report)
		if err := validateHotloadReport(data); err == nil {
			t.Errorf("accepted missing %s", name)
		}
		report[name] = value
	}
	if err := validateHotloadReport([]byte(`{"passed":true} trailing`)); err == nil {
		t.Fatal("accepted trailing/invalid report")
	}
}

func TestHotloadOutputIsBoundedButAlwaysDrained(t *testing.T) {
	var output boundedOutput
	data := bytes.Repeat([]byte("x"), 2<<20)
	n, err := output.Write(data)
	if err != nil || n != len(data) || output.Len() != 1<<20 || !output.truncated {
		t.Fatalf("n=%d err=%v retained=%d truncated=%v", n, err, output.Len(), output.truncated)
	}
	if n, err := output.Write([]byte("more")); n != 4 || err != nil || output.Len() != 1<<20 {
		t.Fatalf("second write=%d %v", n, err)
	}
}

func TestHotloadFailureRemovesEmbeddedTemporaryScript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	err := checkCredentialsHotload(context.Background(), config.DshRuntime{NodeBin: filepath.Join(dir, "missing-node"), BinJS: filepath.Join(dir, "missing-dsh")})
	if err == nil {
		t.Fatal("missing runtime accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary script leaked: %v (%v)", entries, err)
	}
}

func TestDshGateIncludesRequiredLiveHotload(t *testing.T) {
	checks := DshChecks(config.DshRuntime{})
	if len(checks) != 8 {
		t.Fatalf("checks=%d want original seven plus hotload", len(checks))
	}
	last := checks[len(checks)-1]
	if last.Name != "credentials-live-hotload" || !last.Required {
		t.Fatalf("hotload is not a required upgrade gate: %+v", last)
	}
}
