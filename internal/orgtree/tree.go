// Package orgtree is the pure algorithm behind the organization structure: ancestry,
// subtrees, cycle and depth validation, and the tag names an account inherits.
//
// It exists as its own package because three layers need the same tree and only one of them
// may look at the database: internal/registry folds inherited tags into the routing snapshot,
// internal/httpapi filters accounts by subtree and validates writes, and the store enforces
// the depth-ordered delete. Keeping the traversal here means the ancestor walk is written
// once — and that it is written defensively, because the data it reads comes from a database
// where a cycle must never be able to hang a request.
package orgtree

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// MaxDepth is the deepest allowed chain of nodes, counting the root as depth 0.
//
// The ancestor chain is expanded once per account when the routing snapshot is built, so its
// cost is bounded by the depth; 16 is far more than any real organization and keeps that
// bound small enough to reason about. Writes that would exceed it are rejected.
const MaxDepth = 16

// Index is a read-optimized view of a set of nodes.
//
// It tolerates whatever the database hands it: nodes in any order, and a parent_id that
// points at a node that is not in the set (an orphan is treated as a root so it is still
// reachable rather than silently dropped). Both of those are reported by Validate.
type Index struct {
	byID map[int64]*domain.OrgNode
	// children maps a parent id to its children; 0 is the key for roots.
	children map[int64][]int64
	roots    []int64
	depth    map[int64]int
	// duplicates records ids that appeared more than once in the input.
	duplicates []int64
	// orphans records nodes whose parent_id names a node outside the set.
	orphans []int64
}

// NewIndex builds the index. It never fails: a malformed input is described by Validate
// instead, so a caller on a hot-ish path (the snapshot build) cannot be blocked by one bad
// row while the admin path can still report it.
func NewIndex(nodes []*domain.OrgNode) *Index {
	ix := &Index{
		byID:     make(map[int64]*domain.OrgNode, len(nodes)),
		children: make(map[int64][]int64, len(nodes)+1),
		depth:    make(map[int64]int, len(nodes)),
	}
	for _, node := range nodes {
		if node == nil || node.ID == 0 {
			continue
		}
		if _, seen := ix.byID[node.ID]; seen {
			ix.duplicates = append(ix.duplicates, node.ID)
			continue
		}
		ix.byID[node.ID] = node
	}
	for _, node := range ix.byID {
		parent := node.ParentIDValue()
		if parent != 0 {
			if _, ok := ix.byID[parent]; !ok {
				// An orphan hangs off a node that is not here: it becomes a root, so the
				// console still shows it (and its own subtree) instead of hiding it.
				ix.orphans = append(ix.orphans, node.ID)
				parent = 0
			}
		}
		ix.children[parent] = append(ix.children[parent], node.ID)
	}
	ix.sortChildren()
	ix.roots = ix.children[0]
	for id := range ix.byID {
		ix.depth[id] = ix.computeDepth(id)
	}
	return ix
}

// sortChildren orders every sibling list the same way: sort_order, then name, then id.
//
// The id tiebreak is what makes the order total: two siblings with equal sort_order and
// equal names cannot happen (sibling names are unique), but an unsorted SQL result would
// otherwise leak into the console's row order and the inherited-tag order.
func (ix *Index) sortChildren() {
	for parent, ids := range ix.children {
		sort.SliceStable(ids, func(i, j int) bool {
			left, right := ix.byID[ids[i]], ix.byID[ids[j]]
			if left.SortOrder != right.SortOrder {
				return left.SortOrder < right.SortOrder
			}
			if left.Name != right.Name {
				return left.Name < right.Name
			}
			return left.ID < right.ID
		})
		ix.children[parent] = ids
	}
}

// computeDepth walks up to the root, counting edges. A cycle stops the walk and yields the
// depth of the longest chain that terminates, which keeps every later traversal finite.
//
// The walk uses the *effective* parent — a parent id that is missing from the set is a root,
// exactly as it is placed in children — so the recorded depth always agrees with the row's
// position in the displayed tree.
func (ix *Index) computeDepth(id int64) int {
	seen := map[int64]struct{}{}
	depth := 0
	for cur := id; cur != 0; {
		if _, repeat := seen[cur]; repeat {
			break
		}
		seen[cur] = struct{}{}
		node := ix.byID[cur]
		if node == nil {
			break
		}
		parent := node.ParentIDValue()
		if parent == 0 {
			break
		}
		if _, present := ix.byID[parent]; !present {
			break
		}
		depth++
		cur = parent
	}
	return depth
}

// Node returns one node, or nil.
func (ix *Index) Node(id int64) *domain.OrgNode {
	if ix == nil {
		return nil
	}
	return ix.byID[id]
}

// Len reports how many nodes the index holds.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.byID)
}

// Depth returns the node's distance from its root (a root is 0). An unknown id is -1.
func (ix *Index) Depth(id int64) int {
	if ix == nil {
		return -1
	}
	depth, ok := ix.depth[id]
	if !ok {
		return -1
	}
	return depth
}

// Chain returns the node's ancestry from its root down to the node itself, inclusive. It is
// the order in which inherited tags are collected, so the root's tags come first.
//
// A cycle cannot make this loop forever: visited ids are skipped, so the walk always ends.
func (ix *Index) Chain(id int64) []int64 {
	if ix == nil {
		return nil
	}
	up := make([]int64, 0, 4)
	seen := map[int64]struct{}{}
	for cur := id; cur != 0; {
		if _, repeat := seen[cur]; repeat {
			break
		}
		node := ix.byID[cur]
		if node == nil {
			break
		}
		seen[cur] = struct{}{}
		up = append(up, cur)
		cur = node.ParentIDValue()
	}
	out := make([]int64, 0, len(up))
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i])
	}
	return out
}

// Descendants returns the node and everything below it, in depth-first order. The node
// itself is included: callers filter "this node and its subtree" far more often than "only
// the children".
func (ix *Index) Descendants(id int64) []int64 {
	if ix == nil {
		return nil
	}
	if _, ok := ix.byID[id]; !ok {
		return nil
	}
	out := make([]int64, 0, 8)
	visited := map[int64]struct{}{}
	stack := []int64{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := visited[cur]; seen {
			continue
		}
		visited[cur] = struct{}{}
		out = append(out, cur)
		// Push in reverse so children are visited in their declared order.
		kids := ix.children[cur]
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, kids[i])
		}
	}
	return out
}

// SubtreeHeight returns the number of edges from id down to its deepest descendant
// (a leaf is 0, an unknown id is 0).
func (ix *Index) SubtreeHeight(id int64) int {
	if ix == nil {
		return 0
	}
	if _, ok := ix.byID[id]; !ok {
		return 0
	}
	height := 0
	for _, descendant := range ix.Descendants(id) {
		if descendant == id {
			continue
		}
		if d := ix.Depth(descendant) - ix.Depth(id); d > height {
			height = d
		}
	}
	return height
}

// WouldCreateCycle reports whether making newParent the parent of id would produce a cycle.
// newParent 0 (no parent, i.e. a root) never does.
//
// This is the check that turns a destructive structural mistake into a 400 instead of a tree
// that can no longer be rendered: moving a node under its own descendant detaches that
// subtree from every root.
func (ix *Index) WouldCreateCycle(id, newParent int64) bool {
	if ix == nil || newParent == 0 {
		return false
	}
	for _, descendant := range ix.Descendants(id) {
		if descendant == newParent {
			return true
		}
	}
	return false
}

// Ordered returns every node exactly once in display order: depth-first from each root in
// sibling order. A node that is unreachable from any root (a cycle the database should never
// contain) is appended afterwards in id order rather than disappearing from the console.
func (ix *Index) Ordered() []*domain.OrgNode {
	if ix == nil {
		return nil
	}
	out := make([]*domain.OrgNode, 0, len(ix.byID))
	visited := map[int64]struct{}{}
	var walk func(id int64)
	walk = func(id int64) {
		if _, seen := visited[id]; seen {
			return
		}
		node := ix.byID[id]
		if node == nil {
			return
		}
		visited[id] = struct{}{}
		out = append(out, node)
		for _, child := range ix.children[id] {
			walk(child)
		}
	}
	for _, root := range ix.roots {
		walk(root)
	}
	if len(out) != len(ix.byID) {
		rest := make([]int64, 0, len(ix.byID)-len(out))
		for id := range ix.byID {
			if _, seen := visited[id]; !seen {
				rest = append(rest, id)
			}
		}
		sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
		for _, id := range rest {
			walk(id)
		}
	}
	return out
}

// InheritedTagNames resolves, per account, the tag names its memberships grant it.
//
// The result is the union of the ancestor chains of every node the account sits in, in a
// deterministic order: node ids ascending, and within one node root-first. Names are
// de-duplicated keeping the first occurrence, so a tag attached to both a parent and a child
// does not appear twice and cannot change its evaluation position.
//
// This is deliberately computed once per snapshot build rather than per request: it is the
// reason an organization membership costs nothing on the request path.
func (ix *Index) InheritedTagNames(members map[int64][]int64) map[int64][]string {
	out := make(map[int64][]string, len(members))
	if ix == nil {
		return out
	}
	for accountID, nodeIDs := range members {
		if accountID == 0 || len(nodeIDs) == 0 {
			continue
		}
		ordered := make([]int64, 0, len(nodeIDs))
		seenNode := map[int64]struct{}{}
		for _, nodeID := range nodeIDs {
			if _, repeat := seenNode[nodeID]; repeat {
				continue
			}
			seenNode[nodeID] = struct{}{}
			ordered = append(ordered, nodeID)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

		names := make([]string, 0, 4)
		seenName := map[string]struct{}{}
		for _, nodeID := range ordered {
			for _, ancestorID := range ix.Chain(nodeID) {
				node := ix.byID[ancestorID]
				if node == nil {
					continue
				}
				for _, name := range NodeTagNames(node) {
					if _, repeat := seenName[name]; repeat {
						continue
					}
					seenName[name] = struct{}{}
					names = append(names, name)
				}
			}
		}
		if len(names) > 0 {
			out[accountID] = names
		}
	}
	return out
}

// NodeTagNames decodes a node's tag names the same way accounts are decoded: a malformed or
// non-array value yields no names instead of an error, because a bad label must not be able
// to take the whole snapshot (and therefore the data plane) down.
func NodeTagNames(node *domain.OrgNode) []string {
	if node == nil || strings.TrimSpace(node.TagsJSON) == "" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(node.TagsJSON), &names); err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Validate reports every structural problem in one error: duplicate ids, a parent that is
// not part of the set, a cycle, duplicate sibling names, and a chain deeper than MaxDepth.
//
// It is an assertion about data the database is supposed to make impossible, so it exists to
// make a corrupt tree loud (a test, a repair tool, a diagnostic endpoint) rather than to
// guard a write — writes validate their own specific mistake with a specific 400.
func (ix *Index) Validate() error {
	if ix == nil {
		return nil
	}
	var problems []string
	if len(ix.duplicates) > 0 {
		problems = append(problems, fmt.Sprintf("duplicate node ids %v", ix.duplicates))
	}
	for _, id := range ix.orphans {
		node := ix.byID[id]
		problems = append(problems, fmt.Sprintf("node %d (%s) has parent %d, which is not part of the tree",
			id, node.Name, node.ParentIDValue()))
	}
	for id, depth := range ix.depth {
		if depth > MaxDepth {
			problems = append(problems, fmt.Sprintf("node %d (%s) is at depth %d, above the limit of %d",
				id, ix.byID[id].Name, depth, MaxDepth))
		}
	}
	problems = append(problems, ix.cycleProblems()...)
	problems = append(problems, ix.siblingNameProblems()...)
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("org tree is inconsistent: %s", strings.Join(problems, "; "))
}

// cycleProblems reports every node that cannot reach a root.
//
// A cycle is defined by what the walk-up does, not by comparing lengths: the walk ends either
// at a root (parent 0 or a parent outside the set) — a healthy node — or by revisiting a node
// it already passed, which is precisely a cycle. Comparing a walk length against the recorded
// depth would miss a cycle whose members happen to be equally deep.
func (ix *Index) cycleProblems() []string {
	var problems []string
	for id := range ix.byID {
		seen := map[int64]struct{}{}
		cyclic := false
		for cur := id; cur != 0; {
			if _, repeat := seen[cur]; repeat {
				cyclic = true
				break
			}
			seen[cur] = struct{}{}
			node := ix.byID[cur]
			if node == nil {
				break
			}
			parent := node.ParentIDValue()
			if parent == 0 {
				break
			}
			if _, ok := ix.byID[parent]; !ok {
				break
			}
			cur = parent
		}
		if cyclic {
			problems = append(problems, fmt.Sprintf("node %d (%s) is part of a parent cycle", id, ix.byID[id].Name))
		}
	}
	sort.Strings(problems)
	return problems
}

// siblingNameProblems reports names that repeat under one parent.
func (ix *Index) siblingNameProblems() []string {
	var problems []string
	for parent, ids := range ix.children {
		names := map[string]int64{}
		for _, id := range ids {
			node := ix.byID[id]
			if other, repeat := names[node.Name]; repeat {
				problems = append(problems, fmt.Sprintf("nodes %d and %d share the name %q under parent %d",
					other, id, node.Name, parent))
				continue
			}
			names[node.Name] = id
		}
	}
	sort.Strings(problems)
	return problems
}
