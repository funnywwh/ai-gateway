package orgtree

import (
	"slices"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// node builds a node with a parent given as a plain value (0 = root).
func node(id, parent int64, name string, sortOrder int, tags ...string) *domain.OrgNode {
	n := &domain.OrgNode{ID: id, Name: name, SortOrder: sortOrder}
	if parent != 0 {
		parentID := parent
		n.ParentID = &parentID
	}
	if len(tags) > 0 {
		n.TagsJSON = `["` + strings.Join(tags, `","`) + `"]`
	}
	return n
}

// sample is the shape the console has to render and the resolver has to walk:
//
//	总部 (1) ── 研发部 (2) ── 平台组 (4)
//	       └── 市场部 (3)
//	分公司 (5) ── 研发部 (6)          ← same name as (2), a different parent: legal
func sample() []*domain.OrgNode {
	return []*domain.OrgNode{
		node(1, 0, "总部", 100, "internal"),
		node(2, 1, "研发部", 100, "vip"),
		node(3, 1, "市场部", 200),
		node(4, 2, "平台组", 100, "free"),
		node(5, 0, "分公司", 200),
		node(6, 5, "研发部", 100),
	}
}

func TestIndexDepthAndChain(t *testing.T) {
	ix := NewIndex(sample())
	if got := ix.Depth(1); got != 0 {
		t.Fatalf("root depth = %d, want 0", got)
	}
	if got := ix.Depth(4); got != 2 {
		t.Fatalf("平台组 depth = %d, want 2", got)
	}
	if got := ix.Depth(999); got != -1 {
		t.Fatalf("unknown id depth = %d, want -1", got)
	}
	if got := ix.Chain(4); !slices.Equal(got, []int64{1, 2, 4}) {
		t.Fatalf("chain(4) = %v, want [1 2 4] (root first)", got)
	}
	if got := ix.Chain(999); len(got) != 0 {
		t.Fatalf("chain of an unknown node = %v, want empty", got)
	}
}

func TestDescendantsIncludeTheNodeItself(t *testing.T) {
	ix := NewIndex(sample())
	if got := ix.Descendants(1); !slices.Equal(got, []int64{1, 2, 4, 3}) {
		t.Fatalf("descendants(1) = %v, want [1 2 4 3] in depth-first sibling order", got)
	}
	if got := ix.Descendants(4); !slices.Equal(got, []int64{4}) {
		t.Fatalf("descendants of a leaf = %v, want [4]", got)
	}
	if got := ix.Descendants(999); len(got) != 0 {
		t.Fatalf("descendants of an unknown node = %v, want empty", got)
	}
}

func TestSubtreeHeight(t *testing.T) {
	ix := NewIndex(sample())
	if got := ix.SubtreeHeight(1); got != 2 {
		t.Fatalf("height(1) = %d, want 2", got)
	}
	if got := ix.SubtreeHeight(2); got != 1 {
		t.Fatalf("height(2) = %d, want 1", got)
	}
	if got := ix.SubtreeHeight(4); got != 0 {
		t.Fatalf("height of a leaf = %d, want 0", got)
	}
}

func TestWouldCreateCycle(t *testing.T) {
	ix := NewIndex(sample())
	cases := []struct {
		id, newParent int64
		want          bool
		why           string
	}{
		{1, 1, true, "a node cannot be its own parent"},
		{1, 4, true, "moving a node under its own descendant detaches the subtree"},
		{4, 1, false, "moving a node up to its current ancestor is legal"},
		{4, 0, false, "becoming a root is always legal"},
		{3, 5, false, "an unrelated node is legal"},
	}
	for _, tc := range cases {
		if got := ix.WouldCreateCycle(tc.id, tc.newParent); got != tc.want {
			t.Errorf("WouldCreateCycle(%d, %d) = %v, want %v (%s)", tc.id, tc.newParent, got, tc.want, tc.why)
		}
	}
}

func TestWouldCreateCycleIgnoresSelfParentWhenMakingRoot(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{node(1, 0, "only", 100)})
	if ix.WouldCreateCycle(1, 0) {
		t.Fatal("making the only node a root must not count as a cycle")
	}
}

func TestOrderedIsDepthFirstInSiblingOrder(t *testing.T) {
	ix := NewIndex(sample())
	got := []int64{}
	for _, n := range ix.Ordered() {
		got = append(got, n.ID)
	}
	// 总部 subtree first (sort_order 100 before 分公司's 200), each parent before its
	// children: a console can render this by indenting runs of increasing depth.
	if want := []int64{1, 2, 4, 3, 5, 6}; !slices.Equal(got, want) {
		t.Fatalf("ordered = %v, want %v", got, want)
	}
}

func TestOrderedSurvivesACycleByStillListingEveryNode(t *testing.T) {
	// The database should never contain this, but a tree that silently drops rows is a worse
	// failure than one that shows them at the end.
	ix := NewIndex([]*domain.OrgNode{node(1, 2, "a", 100), node(2, 1, "b", 100), node(3, 0, "root", 100)})
	ordered := ix.Ordered()
	if len(ordered) != 3 {
		t.Fatalf("ordered listed %d nodes, want all 3", len(ordered))
	}
	if ordered[0].ID != 3 {
		t.Fatalf("the reachable root must come first, got %d", ordered[0].ID)
	}
}

func TestTraversalTerminatesOnACycle(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{node(1, 2, "a", 100), node(2, 1, "b", 100)})
	// Every one of these would hang or recurse forever without the visited sets.
	if got := ix.Chain(1); len(got) > 2 {
		t.Fatalf("chain on a cycle returned %v; it must stop", got)
	}
	if got := ix.Descendants(1); len(got) > 2 {
		t.Fatalf("descendants on a cycle returned %v; it must stop", got)
	}
	if got := ix.Ordered(); len(got) != 2 {
		t.Fatalf("ordered on a cycle returned %d nodes, want 2", len(got))
	}
}

func TestOrphanParentBecomesARootAndIsReported(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{node(1, 0, "root", 100), node(9, 77, "lost", 100)})
	if got := ix.Depth(9); got != 0 {
		t.Fatalf("an orphan hangs off a missing parent, so it is displayed as a root: depth = %d", got)
	}
	if got := ix.Descendants(0); len(got) != 0 {
		t.Fatalf("id 0 is not a node: %v", got)
	}
	if err := ix.Validate(); err == nil {
		t.Fatal("Validate must report the dangling parent")
	} else if !strings.Contains(err.Error(), "not part of the tree") {
		t.Fatalf("Validate error does not explain the orphan: %v", err)
	}
}

func TestInheritedTagNamesWalksAncestorsRootFirst(t *testing.T) {
	ix := NewIndex(sample())
	members := map[int64][]int64{
		7: {4}, // 平台组 → inherits internal (总部), vip (研发部), free (平台组)
		8: {3}, // 市场部 → inherits internal (总部) only
	}
	got := ix.InheritedTagNames(members)
	if want := []string{"internal", "vip", "free"}; !slices.Equal(got[7], want) {
		t.Fatalf("account 7 inherits %v, want %v (root first, so a parent tag is evaluated before a child's)", got[7], want)
	}
	if want := []string{"internal"}; !slices.Equal(got[8], want) {
		t.Fatalf("account 8 inherits %v, want %v", got[8], want)
	}
}

func TestInheritedTagNamesWithSeveralMemberships(t *testing.T) {
	ix := NewIndex(sample())
	// An account in two nodes gets both chains. Node ids ascend so the order is reproducible
	// rather than dependent on map iteration.
	got := ix.InheritedTagNames(map[int64][]int64{7: {6, 4}})
	if want := []string{"internal", "vip", "free"}; !slices.Equal(got[7], want) {
		t.Fatalf("account 7 inherits %v, want %v", got[7], want)
	}
	// Reversing the input must not change the answer.
	got = ix.InheritedTagNames(map[int64][]int64{7: {4, 6}})
	if want := []string{"internal", "vip", "free"}; !slices.Equal(got[7], want) {
		t.Fatalf("membership order changed the result: %v, want %v", got[7], want)
	}
}

func TestInheritedTagNamesDeduplicatesRepeatedNames(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{
		node(1, 0, "root", 100, "shared", "root-only"),
		node(2, 1, "child", 100, "shared", "child-only"),
	})
	got := ix.InheritedTagNames(map[int64][]int64{5: {2}})
	// "shared" sits on both the root and the child: it must appear once, at the root's
	// position, so its policy is not merged twice.
	if want := []string{"shared", "root-only", "child-only"}; !slices.Equal(got[5], want) {
		t.Fatalf("account 5 inherits %v, want %v", got[5], want)
	}
}

func TestInheritedTagNamesRepeatedMembershipIsIdempotent(t *testing.T) {
	ix := NewIndex(sample())
	once := ix.InheritedTagNames(map[int64][]int64{7: {4}})
	twice := ix.InheritedTagNames(map[int64][]int64{7: {4, 4}})
	if !slices.Equal(once[7], twice[7]) {
		t.Fatalf("listing the same node twice changed the result: %v vs %v", once[7], twice[7])
	}
}

func TestInheritedTagNamesSkipsUnknownAndEmpty(t *testing.T) {
	ix := NewIndex(sample())
	got := ix.InheritedTagNames(map[int64][]int64{
		7: {999}, // a node id that no longer exists (deleted between reads)
		8: {},    // no membership at all
		0: {4},   // a membership with no account cannot exist; it must not create an entry
	})
	if _, present := got[7]; present {
		t.Fatalf("an unknown node produced %v; a deleted node must contribute nothing", got[7])
	}
	if _, present := got[8]; present {
		t.Fatal("an account with no memberships must not get an entry")
	}
	if _, present := got[0]; present {
		t.Fatal("account id 0 is not an account")
	}
}

func TestNodeTagNamesIgnoresMalformedJSON(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`["a","b"]`, 2},
		{``, 0},
		{`not json`, 0},
		{`{"a":1}`, 0},
		{`["a","","  "]`, 1},
		{` ["a"] `, 1},
	}
	for _, tc := range cases {
		got := NodeTagNames(&domain.OrgNode{TagsJSON: tc.raw})
		if len(got) != tc.want {
			t.Errorf("NodeTagNames(%q) = %v, want %d names", tc.raw, got, tc.want)
		}
	}
	if got := NodeTagNames(nil); got != nil {
		t.Errorf("NodeTagNames(nil) = %v, want nil", got)
	}
}

func TestValidateAcceptsTheSampleTree(t *testing.T) {
	if err := NewIndex(sample()).Validate(); err != nil {
		t.Fatalf("the sample tree must be valid: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	tooDeep := []*domain.OrgNode{}
	for i := int64(1); i <= MaxDepth+3; i++ {
		parent := i - 1
		tooDeep = append(tooDeep, node(i, parent, "n", 100))
	}
	ix := NewIndex(append(tooDeep,
		node(1, 0, "dup-id-and-name", 100), // duplicate id, reported once
	))
	err := ix.Validate()
	if err == nil {
		t.Fatal("Validate accepted a duplicate id and an over-deep chain")
	}
	for _, want := range []string{"duplicate node ids", "above the limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error does not mention %q: %v", want, err)
		}
	}
}

func TestValidateReportsDuplicateSiblingNames(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{node(1, 0, "root", 100), node(2, 1, "dup", 100), node(3, 1, "dup", 200)})
	err := ix.Validate()
	if err == nil {
		t.Fatal("Validate accepted two siblings with the same name")
	}
	if !strings.Contains(err.Error(), "share the name") {
		t.Fatalf("Validate error does not explain the duplicate name: %v", err)
	}
}

func TestValidateReportsACycle(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{node(1, 2, "a", 100), node(2, 1, "b", 100)})
	err := ix.Validate()
	if err == nil {
		t.Fatal("Validate accepted a cycle")
	}
	if !strings.Contains(err.Error(), "parent cycle") {
		t.Fatalf("Validate error does not name the cycle: %v", err)
	}
}

func TestSiblingOrderIsTotal(t *testing.T) {
	// Equal sort_order, distinct names: the name decides. This is what keeps the console's
	// row order and the inherited-tag order stable instead of inheriting SQL row order.
	ix := NewIndex([]*domain.OrgNode{
		node(1, 0, "root", 100),
		node(3, 1, "beta", 100),
		node(2, 1, "alpha", 100),
	})
	got := []int64{}
	for _, n := range ix.Ordered() {
		got = append(got, n.ID)
	}
	if want := []int64{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("ordered = %v, want %v", got, want)
	}
}

func TestNewIndexToleratesNilAndZeroIDEntries(t *testing.T) {
	ix := NewIndex([]*domain.OrgNode{nil, {ID: 0, Name: "zero"}, node(1, 0, "root", 100)})
	if ix.Len() != 1 {
		t.Fatalf("index holds %d nodes, want only the one with a real id", ix.Len())
	}
	if err := ix.Validate(); err != nil {
		t.Fatalf("nil/zero-id entries must not make the tree invalid: %v", err)
	}
}
