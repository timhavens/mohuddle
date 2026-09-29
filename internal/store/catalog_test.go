package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestRoomNamesRemainStableAcrossCreationDeletionAndStores(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Create("project", 1)
	if err != nil {
		t.Fatal(err)
	}
	names, err := s.RoomNames()
	if err != nil || names[first.ID] != "Room 1" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	if _, err := s.DeleteRoom(first.ID); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := New(s.Root())
			if err != nil {
				t.Error(err)
				return
			}
			r, err := other.Create("project", 1)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := other.RoomNames(); err != nil {
				t.Error(err)
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	reopened, _ := New(s.Root())
	names, err = reopened.RoomNames()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if names[id] == "Room 1" || seen[names[id]] || names[id] == "" {
			t.Fatalf("bad numbering: %v", names)
		}
		seen[names[id]] = true
		for _, selector := range []string{id, names[id]} {
			resolved, err := reopened.ResolveRoom(selector)
			if err != nil || resolved.ID != id {
				t.Fatalf("resolve %s: %v", selector, err)
			}
		}
	}
	for i := 2; i <= n+1; i++ {
		if _, err := reopened.ResolveRoom(fmt.Sprintf("rOoM%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reopened.ResolveRoom("room1"); err == nil {
		t.Fatal("deleted room name was reused")
	}
}
