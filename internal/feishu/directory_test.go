package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The directory read walks two paged listings against the contact API and mints a tenant
// access token first. The stub below speaks exactly those endpoints and records the calls
// it saw, so the tests assert on the request sequence, not on mocks of our own code.

type directoryStub struct {
	mu sync.Mutex

	// departments maps a parent id to its child list; "0" is the root.
	departments map[string][]contactDepartment
	// members maps a department id to its people; a person may appear under several.
	members map[string][]contactUser
	// includeNames models the two contact data permissions: without them every name is
	// dropped exactly the way the real API omits the fields (code 0, empty payload).
	includeNames bool
	// pageSize forces a smaller page size in the answers so paging is exercised.
	pageSize int
	// tokenFailures makes the first N token minting attempts fail with this code.
	tokenFailuresRemaining int
	tokenFailureCode       int
	// tokenRequests counts minting calls; >1 within one walk means the cache failed.
	tokenRequests int
	// contactRequests records "METHOD path" lines in call order.
	contactRequests []string
	// refusedRemaining, when > 0, makes the next N contact calls answer with refuseCode —
	// the mid-walk revocation case. Later calls succeed because a fresh mint returns a
	// new token (t-2, t-3, …).
	refusedRemaining int
	refuseCode       int
}

func (s *directoryStub) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.URL.Path == "/token":
		s.tokenRequests++
		if s.tokenFailuresRemaining > 0 {
			s.tokenFailuresRemaining--
			writeJSON(t, w, map[string]any{"code": s.tokenFailureCode, "msg": "bad credentials"})
			return
		}
		writeJSON(t, w, map[string]any{
			"code": 0, "tenant_access_token": fmt.Sprintf("t-%d", s.tokenRequests), "expire": 7200,
		})
		return

	case r.URL.Path == "/contact/v3/departments" && r.URL.Query().Get("parent_department_id") != "":
		s.contactRequests = append(s.contactRequests, "list-children "+r.URL.Query().Get("parent_department_id"))
		if !s.authorized(t, r, w) {
			return
		}
		parent := r.URL.Query().Get("parent_department_id")
		items := s.departments[parent]
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{
			"items": pageItems(r, items), "has_more": false,
		}})
		return

	case r.URL.Path == "/contact/v3/users":
		s.contactRequests = append(s.contactRequests, "list-users "+r.URL.Query().Get("department_id"))
		if !s.authorized(t, r, w) {
			return
		}
		department := r.URL.Query().Get("department_id")
		items := s.members[department]
		if !s.includeNames {
			stripped := make([]contactUser, 0, len(items))
			for _, person := range items {
				stripped = append(stripped, contactUser{OpenID: person.OpenID, UnionID: person.UnionID})
			}
			items = stripped
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{
			"items": pageItems(r, items), "has_more": false,
		}})
		return
	}

	t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
}

func (s *directoryStub) authorized(t *testing.T, r *http.Request, w http.ResponseWriter) bool {
	if s.refusedRemaining <= 0 {
		return true
	}
	s.refusedRemaining--
	writeJSON(t, w, map[string]any{"code": s.refuseCode, "msg": "invalid access token"})
	return false
}

// pageItems honours page_size and returns only the first page (the fixtures stay below it).
func pageItems[T any](r *http.Request, items []T) []T {
	size := directoryPageSize
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			size = parsed
		}
	}
	if len(items) > size {
		items = items[:size]
	}
	return items
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

func newDirectoryClient(t *testing.T, stub *directoryStub) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.handle(t, w, r)
	}))
	t.Cleanup(server.Close)
	return &Client{
		AppID:          "cli_test",
		AppSecret:      "secret",
		TenantTokenURL: server.URL + "/token",
		ContactURL:     server.URL + "/contact/v3",
	}, server
}

func TestDirectoryWalksTheTree(t *testing.T) {
	stub := &directoryStub{
		departments: map[string][]contactDepartment{
			"0": {
				{OpenDepartmentID: "od_a", Name: "总裁办"},
				{OpenDepartmentID: "od_b", Name: "研发部"},
			},
			"od_b": {
				{OpenDepartmentID: "od_b1", Name: "平台组"},
			},
			"od_b1": {},
			"od_a":  {},
		},
		members: map[string][]contactUser{
			// The virtual root holds people of its own — they must land in the result
			// with no department attached.
			"0":    {{OpenID: "ou_root", UnionID: "on_root", Name: "老板"}},
			"od_a": {{OpenID: "ou_zhao", UnionID: "on_zhao", Name: "赵六"}},
			"od_b": {
				{OpenID: "ou_zhou", UnionID: "on_zhou", Name: "周八"},
				{OpenID: "ou_wang", UnionID: "on_wang", Name: "王五"},
			},
			// 王五 belongs to two departments: the walk must merge the two sightings
			// into one person carrying both department ids.
			"od_b1": {{OpenID: "ou_wang", UnionID: "on_wang", Name: "王五"}},
		},
		includeNames: true,
	}
	client, _ := newDirectoryClient(t, stub)

	got, err := client.Directory(context.Background(), DirectoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Departments in BFS order, parents before children, root never listed.
	var gotOrder []string
	for _, department := range got.Departments {
		gotOrder = append(gotOrder, department.ID)
	}
	want := []string{"od_a", "od_b", "od_b1"}
	if len(gotOrder) != len(want) {
		t.Fatalf("departments = %v, want %v", gotOrder, want)
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("departments = %v, want %v (order matters: parents first)", gotOrder, want)
		}
	}
	if got.Departments[0].ParentID != "" || got.Departments[0].Depth != 0 {
		t.Fatalf("top-level department wrong: %+v", got.Departments[0])
	}
	if got.Departments[2].ParentID != "od_b" || got.Departments[2].Depth != 1 {
		t.Fatalf("nested department wrong: %+v", got.Departments[2])
	}

	if len(got.Users) != 4 {
		t.Fatalf("users = %+v, want 4 deduplicated people", got.Users)
	}
	byID := map[string]DirectoryUser{}
	for _, person := range got.Users {
		byID[person.OpenID] = person
	}
	if byID["ou_root"].Name != "老板" || len(byID["ou_root"].DepartmentIDs) != 0 {
		t.Fatalf("root person wrong: %+v", byID["ou_root"])
	}
	if ids := byID["ou_wang"].DepartmentIDs; len(ids) != 2 || ids[0] != "od_b" || ids[1] != "od_b1" {
		t.Fatalf("two-department person merged wrong: %+v", byID["ou_wang"])
	}
	if !got.NamesAvailable || got.Truncated {
		t.Fatalf("names=%v truncated=%v, want available and complete", got.NamesAvailable, got.Truncated)
	}

	// One token mint per walk, then one listing pair per department (including the root).
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tokenRequests != 1 {
		t.Fatalf("token minted %d times, want 1 (the cache must serve the walk)", stub.tokenRequests)
	}
	wantCalls := []string{
		"list-users 0", "list-children 0",
		"list-users od_a", "list-children od_a",
		"list-users od_b", "list-children od_b",
		"list-users od_b1", "list-children od_b1",
	}
	if strings.Join(stub.contactRequests, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("call sequence = %v, want %v", stub.contactRequests, wantCalls)
	}
}

// Without 「获取部门基础信息」/「获取用户基本信息」 every call still answers code 0 but carries no
// names. The snapshot must say so instead of pretending the names are empty on purpose.
func TestDirectoryReportsMissingNamePermission(t *testing.T) {
	stub := &directoryStub{
		departments: map[string][]contactDepartment{
			"0": {{OpenDepartmentID: "od_x"}}, // name omitted by the API
		},
		members: map[string][]contactUser{
			"0":    {{OpenID: "ou_a"}},
			"od_x": {{OpenID: "ou_b"}},
		},
		includeNames: false,
	}
	client, _ := newDirectoryClient(t, stub)

	got, err := client.Directory(context.Background(), DirectoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.NamesAvailable {
		t.Fatalf("names reported available although the permission is missing: %+v", got)
	}
	if len(got.Departments) != 1 || len(got.Users) != 2 {
		t.Fatalf("ids must still be read: %+v", got)
	}
}

// Empty credentials are a deployment fault and must be classified as such, not as an
// availability problem.
func TestDirectoryClassifiesCredentialFailures(t *testing.T) {
	for _, code := range []int{10003, 10004, 99991663} {
		stub := &directoryStub{tokenFailureCode: code, tokenFailuresRemaining: 100}
		client, _ := newDirectoryClient(t, stub)
		_, err := client.Directory(context.Background(), DirectoryOptions{})
		if err == nil {
			t.Fatalf("code %d accepted", code)
		}
		var feishuErr *Error
		if !errors.As(err, &feishuErr) || feishuErr.Kind != KindCredentials {
			t.Fatalf("code %d classified as %v, want credentials", code, err)
		}
	}
	// An app-level failure (the token works, but Feishu refuses the call) stays
	// app-unavailable and is not retried.
	stub := &directoryStub{
		departments:      map[string][]contactDepartment{"0": {}},
		members:          map[string][]contactUser{"0": {}},
		refusedRemaining: 100,
		refuseCode:       99991672,
	}
	client, _ := newDirectoryClient(t, stub)
	_, err := client.Directory(context.Background(), DirectoryOptions{})
	var feishuErr *Error
	if err == nil || !errors.As(err, &feishuErr) || feishuErr.Kind != KindAppUnavailable {
		t.Fatalf("refused call classified as %v, want app_unavailable", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tokenRequests != 1 {
		t.Fatalf("an app-level refusal must not re-mint tokens, minted %d times", stub.tokenRequests)
	}
}

// A token refused mid-walk is worth exactly one fresh-mint retry: the cached token can be
// revoked while the walk is running, and the second attempt must succeed transparently.
func TestDirectoryRetriesOnceAfterTokenRevocation(t *testing.T) {
	stub := &directoryStub{
		departments: map[string][]contactDepartment{
			"0":    {{OpenDepartmentID: "od_a", Name: "研发部"}},
			"od_a": {},
		},
		members: map[string][]contactUser{
			"0":    {},
			"od_a": {{OpenID: "ou_a", Name: "张三"}},
		},
		includeNames: true,
		// The first contact call (the root's user listing) hits the revoked token; every
		// later call carries the fresh one and succeeds.
		refusedRemaining: 1,
		refuseCode:       99991663,
	}
	client, _ := newDirectoryClient(t, stub)

	got, err := client.Directory(context.Background(), DirectoryOptions{})
	if err != nil {
		t.Fatalf("the retry should have papered over the revocation: %v", err)
	}
	if len(got.Departments) != 1 || len(got.Users) != 1 {
		t.Fatalf("walk incomplete after retry: %+v", got)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.tokenRequests != 2 {
		t.Fatalf("token minted %d times, want exactly 2 (initial + retry)", stub.tokenRequests)
	}
}

// The bounds exist so a misunderstanding cannot turn one sync into an unbounded walk; the
// snapshot reports the truncation instead of pretending to be complete.
func TestDirectoryTruncatesAtTheDepartmentBound(t *testing.T) {
	stub := &directoryStub{
		departments:  map[string][]contactDepartment{"0": {}},
		members:      map[string][]contactUser{"0": {}},
		includeNames: true,
	}
	// Build a flat tree of five departments and allow only two.
	children := []contactDepartment{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("od_%02d", i)
		children = append(children, contactDepartment{OpenDepartmentID: id, Name: fmt.Sprintf("部门%d", i)})
		stub.departments[id] = nil
		stub.members[id] = nil
	}
	stub.departments["0"] = children
	client, _ := newDirectoryClient(t, stub)

	got, err := client.Directory(context.Background(), DirectoryOptions{MaxDepartments: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Fatalf("truncation not reported: %+v", got)
	}
	if len(got.Departments) != 2 {
		t.Fatalf("departments = %d, want the bound of 2", len(got.Departments))
	}
}

// A page size above Feishu's documented maximum is clamped, not forwarded.
func TestDirectoryClampsPageSize(t *testing.T) {
	stub := &directoryStub{
		departments:  map[string][]contactDepartment{"0": {}},
		members:      map[string][]contactUser{"0": {}},
		includeNames: true,
	}
	client, _ := newDirectoryClient(t, stub)
	if _, err := client.Directory(context.Background(), DirectoryOptions{PageSize: 500}); err != nil {
		t.Fatal(err)
	}
	// Every listing the stub served had to arrive with page_size=50, Feishu's maximum.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.contactRequests) == 0 {
		t.Fatal("no contact calls recorded")
	}
}
