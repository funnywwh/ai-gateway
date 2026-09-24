package registry

import "testing"

// M88 retired prefix aliases: the tenant a login lands in is decided by aigw's account mapping,
// so a rotation needs no alias to keep an old key working — the key works for as long as aigw
// keeps it active. What remains is that the label follows the record without leaking the
// previous value into snapshots.
func TestRegistrySnapshotsDoNotExposeMutableLabels(t *testing.T) {
	r := New("unused", "unused-map")
	row := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	row.PreviousPrefixes = []string{"sk-bbbbbbbbb"}
	if err := r.Put(row); err != nil {
		t.Fatal(err)
	}
	row.PreviousPrefixes[0] = "sk-ccccccccc"
	got, _ := r.Get("alice")
	got.PreviousPrefixes[0] = "sk-ddddddddd"
	list := r.List()
	list[0].PreviousPrefixes[0] = "sk-eeeeeeeee"
	again, _ := r.Get("alice")
	if again.PreviousPrefixes[0] != "sk-bbbbbbbbb" {
		t.Fatalf("caller mutated the canonical registry: %+v", again.PreviousPrefixes)
	}
}
