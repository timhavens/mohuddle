package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

// Room numbers belong to the store, not a process or the current sort order.
// Deleted entries are retained so a name can never silently refer to new work.
type roomCatalog struct {
	Next    int            `json:"next"`
	Numbers map[string]int `json:"numbers"`
}

func (s *Store) RoomNames() (map[string]string, error) {
	unlock, err := lockCatalog(filepath.Join(s.root, ".catalog.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	path := filepath.Join(s.root, "room_names.json")
	catalog := roomCatalog{Next: 1, Numbers: map[string]int{}}
	if data, err := readFile(path); err == nil {
		if err := json.Unmarshal(data, &catalog); err != nil {
			return nil, fmt.Errorf("read room names: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if catalog.Numbers == nil {
		catalog.Numbers = map[string]int{}
	}
	seen := map[int]bool{}
	for _, n := range catalog.Numbers {
		if n < 1 || seen[n] {
			return nil, fmt.Errorf("invalid room name directory")
		}
		seen[n] = true
		if catalog.Next <= n {
			catalog.Next = n + 1
		}
	}
	if catalog.Next < 1 {
		catalog.Next = 1
	}
	rooms, err := s.ListRooms()
	if err != nil {
		return nil, err
	}
	sort.Slice(rooms, func(i, j int) bool {
		if rooms[i].CreatedAt.Equal(rooms[j].CreatedAt) {
			return rooms[i].ID < rooms[j].ID
		}
		return rooms[i].CreatedAt.Before(rooms[j].CreatedAt)
	})
	changed := false
	for _, r := range rooms {
		if catalog.Numbers[r.ID] == 0 {
			catalog.Numbers[r.ID] = catalog.Next
			catalog.Next++
			changed = true
		}
	}
	if changed {
		data, err := json.MarshalIndent(catalog, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := s.WritePrivateState("room_names.json", data); err != nil {
			return nil, err
		}
	}
	names := make(map[string]string, len(catalog.Numbers))
	for id, n := range catalog.Numbers {
		names[id] = fmt.Sprintf("Room %d", n)
	}
	return names, nil
}

func (s *Store) ResolveRoom(selector string) (chat.Room, error) {
	selector = strings.TrimSpace(selector)
	if validateID(selector) == nil {
		return s.LoadRoom(selector)
	}
	names, err := s.RoomNames()
	if err != nil {
		return chat.Room{}, err
	}
	key := strings.ToLower(strings.ReplaceAll(selector, " ", ""))
	if !strings.HasPrefix(key, "room") {
		return chat.Room{}, fmt.Errorf("unknown room; use /rooms to see available rooms")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(key, "room"))
	if err != nil || n < 1 {
		return chat.Room{}, fmt.Errorf("use a room name such as room2 or an existing room ID")
	}
	for id, name := range names {
		if name == fmt.Sprintf("Room %d", n) {
			return s.LoadRoom(id)
		}
	}
	return chat.Room{}, fmt.Errorf("room not found; use /rooms to see available rooms")
}

// WritePrivateState atomically persists manager metadata without touching rooms.
func (s *Store) WritePrivateState(name string, data []byte) error {
	if filepath.Base(name) != name {
		return fmt.Errorf("invalid state name")
	}
	f, err := os.CreateTemp(s.root, ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(f.Name(), filepath.Join(s.root, name))
}
