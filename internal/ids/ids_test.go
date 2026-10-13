package ids

import (
	"sort"
	"testing"
	"time"
)

func TestIDsAreUniqueAndOrdered(t *testing.T) {
	fixed := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	g := NewGenerator(func() time.Time { return fixed }) // same millisecond for every call
	var list []string
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := g.New("pi")
		if seen[id] || !HasPrefix(id, "pi") {
			t.Fatalf("bad or duplicate id %s", id)
		}
		seen[id] = true
		list = append(list, id)
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("ids from one generator must sort in creation order")
	}
	later := NewGenerator(func() time.Time { return fixed.Add(time.Millisecond) }).New("pi")
	if later <= list[len(list)-1] {
		t.Fatal("a later millisecond must sort after")
	}
}

func TestHasPrefixAndRandom(t *testing.T) {
	id := New("mer")
	if !HasPrefix(id, "mer") || HasPrefix(id, "me") || HasPrefix("mer_short", "mer") || HasPrefix("mer_"+id[4:len(id)-1]+"!", "mer") {
		t.Fatalf("HasPrefix misbehaves for %s", id)
	}
	if len(RandomHex(8)) != 16 || len(RandomToken(5)) != 8 || RandomToken(20) == RandomToken(20) {
		t.Fatal("random helpers wrong")
	}
}
