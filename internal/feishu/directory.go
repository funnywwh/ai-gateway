package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Feishu contact directory (M70): the department tree and the people in it, read with
// the tenant access token of the very same self-built app the identity flows use. Nothing
// here writes back to Feishu — this side of the client only reads the directory.
//
// Two deployment facts shape the traversal (both observed on a real tenant):
//
//   - A person can belong to several departments, and the users listing does not always
//     carry department_ids. So the tree is walked department by department and a person
//     seen in several departments is merged into one entry by open_id — which is also the
//     merge rule the sync applies ("人员id相同合并").
//   - Without the two data permissions 「获取部门基础信息」/「获取用户基本信息」 every call still
//     answers code 0 but omits the name fields. That is not a transient failure, so it is
//     reported as NamesAvailable=false and the console explains which permissions to add
//     instead of rendering blank rows.

const (
	// DefaultTenantTokenURL and DefaultContactURL are Feishu's documented endpoints. They
	// live here (not only in config) so a Client built as a struct literal — every test
	// fixture does exactly that — talks to the real API unless a test points the fields
	// at a stub.
	DefaultTenantTokenURL = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	DefaultContactURL     = "https://open.feishu.cn/open-apis/contact/v3"

	// directoryPageSize is Feishu's documented maximum page size for both listings.
	directoryPageSize = 50
	// directoryMaxPages bounds one paged listing, and directoryMaxDepts bounds the whole
	// tree: a misunderstood payload must not turn one sync into an unbounded walk. Hitting
	// either is reported (Truncated), never papered over.
	directoryMaxPages = 40
	directoryMaxDepts = 500

	// rootDepartmentID is how Feishu addresses "the company itself": it is the parent of
	// every top-level department and can hold members of its own. It is walked for its
	// members but never reported as a department — it has no name and cannot become an
	// org node.
	rootDepartmentID = "0"

	// tokenExpiryMargin retires a cached tenant access token shortly before Feishu does,
	// so a sync that starts near the end of a token's life does not fail halfway through.
	tokenExpiryMargin = 60 * time.Second
)

// Department is one Feishu department. ID is the open_department_id ("od-…"), the stable
// identifier a local org node stores after a merge. ParentID is "" at the top level
// (Feishu parents those under the virtual root, which is not itself a department).
type Department struct {
	ID       string
	ParentID string
	Name     string
	// Depth counts from the top level: a department directly under the root has depth 0,
	// matching how the local org tree numbers its own roots.
	Depth int
}

// DirectoryUser is one person, deduplicated across every department that lists them.
type DirectoryUser struct {
	OpenID        string
	UnionID       string
	Name          string
	DepartmentIDs []string
}

// Directory is one snapshot of the tenant's contact directory. Departments arrive in BFS
// order — a parent always appears before its children, which is what lets the sync create
// org nodes top-down; users arrive in first-seen order.
type Directory struct {
	Departments []Department
	Users       []DirectoryUser
	// NamesAvailable is false when the app lacks the two contact data permissions: the
	// calls succeed (code 0) but every name comes back empty.
	NamesAvailable bool
	// Truncated reports that a bound (MaxDepartments/MaxPages) was hit and the snapshot is
	// deliberately partial.
	Truncated bool
	FetchedAt time.Time
}

// DirectoryOptions bound one walk. Zero fields fall back to the documented defaults.
type DirectoryOptions struct {
	PageSize       int
	MaxDepartments int
	MaxPages       int
}

// tenantToken is the cached tenant access token. Feishu documents the token as reusable
// until expiry, and minting one is itself rate limited — one sync walks dozens of
// departments, so one token per walk is the difference between 1 and 24 token calls.
type tenantToken struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

func (t *tenantToken) get() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value == "" || !time.Now().Before(t.expires) {
		return "", false
	}
	return t.value, true
}

func (t *tenantToken) put(value string, expiresIn time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value = value
	t.expires = time.Now().Add(expiresIn - tokenExpiryMargin)
}

func (t *tenantToken) invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value = ""
	t.expires = time.Time{}
}

// tenantTokenURL falls back to the documented endpoint, so a Client assembled field by
// field behaves like one built by New.
func (c *Client) tenantTokenURL() string {
	if target := strings.TrimSpace(c.TenantTokenURL); target != "" {
		return target
	}
	return DefaultTenantTokenURL
}

// contactBase is the prefix of every contact call.
func (c *Client) contactBase() string {
	if base := strings.TrimSpace(c.ContactURL); base != "" {
		return base
	}
	return DefaultContactURL
}

// tenantAccessToken returns a usable tenant access token, minting one when the cache is
// empty or stale.
func (c *Client) tenantAccessToken(ctx context.Context) (string, error) {
	if token, ok := c.token.get(); ok {
		return token, nil
	}
	body := `{"app_id":` + jsonString(c.AppID) + `,"app_secret":` + jsonString(c.AppSecret) + `}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tenantTokenURL(), strings.NewReader(body))
	if err != nil {
		return "", &Error{Kind: KindUnreachable, Message: "building the tenant token request failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	raw, status, err := c.do(req)
	if err != nil {
		return "", err
	}
	var answer struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", &Error{Kind: KindUnreachable, Message: "the tenant token response was not JSON", Status: status}
	}
	if answer.Code != 0 {
		return "", classifyTenantTokenError(answer.Code, status)
	}
	if strings.TrimSpace(answer.TenantAccessToken) == "" {
		return "", &Error{Kind: KindUnreachable, Message: "the tenant token response carried no token", Status: status}
	}
	expiresIn := time.Duration(answer.Expire) * time.Second
	if expiresIn <= 0 {
		expiresIn = 2 * time.Hour // Feishu's documented default
	}
	c.token.put(answer.TenantAccessToken, expiresIn)
	return answer.TenantAccessToken, nil
}

// classifyTenantTokenError maps the token endpoint's own error codes. Credential problems
// are deployment faults (a wrong App Secret answers 10004); everything else is reported
// with its numeric code, because Feishu's message text is not a contract.
func classifyTenantTokenError(code, status int) error {
	switch code {
	case 10003, 10004, 10005, 10006, 10007, 10008, 99991661, 99991663, 99991668:
		return &Error{Kind: KindCredentials, Message: "Feishu rejected the application credentials", Status: status}
	default:
		return &Error{Kind: KindAppUnavailable,
			Message: fmt.Sprintf("Feishu returned error code %d for the tenant access token", code), Status: status}
	}
}

// classifyContactError maps a failed contact call. Token refusals are separated out because
// they are the one failure a fresh token can fix (see contactGet's single retry); the rest
// keep their numeric code.
func classifyContactError(code, status int) *Error {
	switch code {
	case 99991661, 99991663, 99991668:
		return &Error{Kind: KindCredentials, Message: "the Feishu access token was refused", Status: status}
	default:
		return &Error{Kind: KindAppUnavailable,
			Message: fmt.Sprintf("Feishu returned error code %d for the contact API", code), Status: status}
	}
}

// contactDepartment and contactUser are the shapes both listings return. The users listing
// is documented to carry more fields (email, mobile, …); everything beyond the identity and
// the name is display material the sync does not need, so it is not even declared.
type contactDepartment struct {
	OpenDepartmentID string `json:"open_department_id"`
	Name             string `json:"name"`
}

type contactUser struct {
	OpenID  string `json:"open_id"`
	UnionID string `json:"union_id"`
	Name    string `json:"name"`
}

// contactPage is the envelope every contact listing shares.
type contactPage[T any] struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		HasMore   bool   `json:"has_more"`
		PageToken string `json:"page_token"`
		Items     []T    `json:"items"`
	} `json:"data"`
}

// contactGet performs one GET against the contact API with a bearer tenant token.
// Generic methods do not exist in Go, so this is a free function over *Client.
func contactGet[T any](ctx context.Context, c *Client, path string, params url.Values) (contactPage[T], error) {
	fetch := func(token string) (contactPage[T], int, error) {
		target := strings.TrimRight(c.contactBase(), "/") + path
		if encoded := params.Encode(); encoded != "" {
			target += "?" + encoded
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return contactPage[T]{}, 0, &Error{Kind: KindUnreachable, Message: "building the contact request failed"}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		body, status, err := c.do(req)
		if err != nil {
			return contactPage[T]{}, 0, err
		}
		var answer contactPage[T]
		if err := json.Unmarshal(body, &answer); err != nil {
			return contactPage[T]{}, 0, &Error{Kind: KindUnreachable,
				Message: "the contact response was not JSON", Status: status}
		}
		return answer, status, nil
	}

	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return contactPage[T]{}, err
	}
	answer, status, err := fetch(token)
	if err != nil {
		return contactPage[T]{}, err
	}
	if answer.Code == 0 {
		return answer, nil
	}
	classified := classifyContactError(answer.Code, status)
	// A refused token is worth exactly one retry with a fresh one: the cached value may
	// have been revoked mid-walk. A second refusal is a real answer and is returned.
	if classified.Kind == KindCredentials {
		c.token.invalidate()
		if fresh, tokenErr := c.tenantAccessToken(ctx); tokenErr != nil {
			return contactPage[T]{}, classified
		} else if retried, retryStatus, retryErr := fetch(fresh); retryErr != nil {
			return contactPage[T]{}, retryErr
		} else if retried.Code == 0 {
			return retried, nil
		} else {
			return contactPage[T]{}, classifyContactError(retried.Code, retryStatus)
		}
	}
	return contactPage[T]{}, classified
}

// contactList pages one contact listing to its end (or the page bound), feeding every item
// to consume. The returned flag reports an early stop, so the caller marks the snapshot
// truncated instead of presenting a partial walk as complete.
func contactList[T any](ctx context.Context, c *Client, path string, query url.Values, maxPages int, consume func(T)) (bool, error) {
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return true, nil
		}
		params := url.Values{}
		for key, values := range query {
			for _, value := range values {
				params.Set(key, value)
			}
		}
		params.Set("page_size", strconv.Itoa(directoryPageSize))
		if cursor != "" {
			// Feishu hands the token back percent-encoded and expects it sent back intact;
			// url.Values re-encodes it byte-identically, which is what the API requires.
			params.Set("page_token", cursor)
		}
		answer, err := contactGet[T](ctx, c, path, params)
		if err != nil {
			return false, err
		}
		for _, item := range answer.Data.Items {
			consume(item)
		}
		if !answer.Data.HasMore || answer.Data.PageToken == "" {
			return false, nil
		}
		cursor = answer.Data.PageToken
	}
}

// Directory walks the whole visible department tree and returns the departments in BFS
// order plus every person, deduplicated by open_id.
//
// The walk is sequential on purpose: one sync touches (departments + 1) listings, and
// Feishu rate limits per app. Sequential keeps a mid-size tenant (here: 23 departments,
// 87 people) at about two dozen cheap calls instead of risking a burst.
func (c *Client) Directory(ctx context.Context, opts DirectoryOptions) (Directory, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 || pageSize > directoryPageSize {
		pageSize = directoryPageSize
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = directoryMaxPages
	}
	maxDepartments := opts.MaxDepartments
	if maxDepartments <= 0 {
		maxDepartments = directoryMaxDepts
	}

	out := Directory{FetchedAt: time.Now().UTC()}
	usersByOpenID := map[string]int{} // open_id → index into out.Users

	// The pending entry carries its parent's id and its own name so each department row
	// can be emitted the moment it is dequeued — which is what keeps the slice BFS-ordered
	// without a sort and without a second lookup per department.
	type pending struct {
		id, parentID string
		name         string
		depth        int
	}
	queue := []pending{{id: rootDepartmentID, depth: -1}}

	mergeUser := func(person contactUser, departmentID string) {
		if strings.TrimSpace(person.OpenID) == "" {
			return
		}
		if at, seen := usersByOpenID[person.OpenID]; seen {
			if departmentID != "" {
				out.Users[at].DepartmentIDs = append(out.Users[at].DepartmentIDs, departmentID)
			}
			return
		}
		ids := []string{}
		if departmentID != "" {
			ids = append(ids, departmentID)
		}
		usersByOpenID[person.OpenID] = len(out.Users)
		out.Users = append(out.Users, DirectoryUser{
			OpenID: person.OpenID, UnionID: person.UnionID, Name: person.Name, DepartmentIDs: ids,
		})
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		departmentID := ""
		if current.depth >= 0 {
			departmentID = current.id
		}

		// The people of this department, however many pages it takes.
		members := url.Values{}
		members.Set("department_id", current.id)
		members.Set("user_id_type", "open_id")
		members.Set("department_id_type", "open_department_id")
		truncated, err := contactList[contactUser](ctx, c, "/users", members, maxPages, func(person contactUser) {
			mergeUser(person, departmentID)
		})
		if err != nil {
			return Directory{}, err
		}
		if truncated {
			out.Truncated = true
			return out, nil
		}

		// The child departments. Their listing carries the name, so no per-department
		// detail call is needed — which is also why a missing 「获取部门基础信息」 permission
		// shows up as empty names rather than as an error.
		children := url.Values{}
		children.Set("parent_department_id", current.id)
		children.Set("department_id_type", "open_department_id")
		kids := []contactDepartment{}
		truncated, err = contactList[contactDepartment](ctx, c, "/departments", children, maxPages, func(department contactDepartment) {
			if strings.TrimSpace(department.OpenDepartmentID) != "" {
				kids = append(kids, department)
			}
		})
		if err != nil {
			return Directory{}, err
		}
		if truncated {
			out.Truncated = true
			return out, nil
		}

		if current.depth >= 0 {
			if len(out.Departments) >= maxDepartments {
				out.Truncated = true
				return out, nil
			}
			out.Departments = append(out.Departments, Department{
				ID: current.id, ParentID: current.parentID, Name: current.name, Depth: current.depth,
			})
		}
		for _, kid := range kids {
			queue = append(queue, pending{
				id: kid.OpenDepartmentID, parentID: departmentKey(current.id), name: kid.Name,
				depth: current.depth + 1,
			})
		}
	}

	// Names are "available" when at least one object carried one. A completely empty
	// directory is neither available nor unavailable — nothing was read, so there is no
	// missing-permission signal to report.
	namesSeen := false
	for _, department := range out.Departments {
		if strings.TrimSpace(department.Name) != "" {
			namesSeen = true
		}
	}
	for _, user := range out.Users {
		if strings.TrimSpace(user.Name) != "" {
			namesSeen = true
		}
	}
	out.NamesAvailable = namesSeen || (len(out.Departments) == 0 && len(out.Users) == 0)
	return out, nil
}

// departmentKey maps the virtual root to "": a department whose parent is the root is a
// top-level department, and a top-level department has no local parent node.
func departmentKey(id string) string {
	if id == rootDepartmentID {
		return ""
	}
	return id
}

// jsonString encodes one string as a JSON string literal for a hand-built request body.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
