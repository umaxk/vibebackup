package service

import (
	"testing"
)

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
