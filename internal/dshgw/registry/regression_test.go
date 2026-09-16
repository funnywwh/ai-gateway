package registry

import "testing"

func TestSamePrefixRotationRevokesOldAliases(t *testing.T) {
	r := New("unused", "unused-map")
	row := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	row.PreviousPrefixes = []string{"sk-bbbbbbbbb"}
	if err := r.Put(row); err != nil {
		t.Fatal(err)
	}
	if err := r.RotatePrefix("alice", "sk-aaaaaaaaa", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ByPrefix("sk-bbbbbbbbb"); ok {
		t.Fatal("old alias survives explicit revocation")
	}
}

func TestRotateBackToAliasDoesNotDuplicatePrefix(t *testing.T) {
	r := New("unused", "unused-map")
	row := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	row.PreviousPrefixes = []string{"sk-bbbbbbbbb"}
	if err := r.Put(row); err != nil {
		t.Fatal(err)
	}
	if err := r.RotatePrefix("alice", "sk-bbbbbbbbb", true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("alice")
	if got.KeyPrefix != "sk-bbbbbbbbb" || len(got.PreviousPrefixes) != 1 || got.PreviousPrefixes[0] != "sk-aaaaaaaaa" {
		t.Fatalf("unexpected prefix history: %+v", got)
	}
}

func TestRegistrySnapshotsDoNotExposeMutableAliases(t *testing.T) {
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
	if _, ok := r.ByPrefix("sk-bbbbbbbbb"); !ok {
		t.Fatal("caller mutated canonical registry aliases")
	}
}
