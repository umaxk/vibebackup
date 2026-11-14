package service

import (
	"testing"

	"vbackuper/internal/store"
)

func TestSlug(t *testing.T) {
	s := &Service{}
	cases := []struct {
		name string
		want string
	}{
		{"Фото и документы", "foto-i-dokumenty-7"},
		{"  DB / Main  ", "db-main-7"},
		{"", "target-7"},
	}
	for _, c := range cases {
		got := s.slug(&store.Target{ID: 7, Name: c.name})
		if got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCleanList(t *testing.T) {
	got := cleanList([]string{"node_modules, *.log\n.cache;tmp", "", "\n"})
	want := []string{"node_modules", "*.log", ".cache", "tmp"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTriggerLabel(t *testing.T) {
	if triggerLabel("manual") != "вручную" {
		t.Fatal("unexpected manual label")
	}
	if triggerLabel("schedule") != "по расписанию" {
		t.Fatal("unexpected schedule label")
	}
}
