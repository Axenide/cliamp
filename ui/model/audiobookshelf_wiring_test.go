package model

import "testing"

func TestProviderKeyForShortcutB(t *testing.T) {
	if got := providerKeyForShortcut("B"); got != "audiobookshelf" {
		t.Fatalf("providerKeyForShortcut(\"B\") = %q, want audiobookshelf", got)
	}
}

func TestAudiobookshelfEmptyStateHint(t *testing.T) {
	hint, ok := providerEmptyStateHint["audiobookshelf"]
	if !ok {
		t.Fatal("providerEmptyStateHint has no audiobookshelf entry")
	}
	if hint == "" {
		t.Fatal("audiobookshelf hint is empty")
	}
}

func TestAudiobookshelfCommandRegistryEntry(t *testing.T) {
	for _, c := range commandRegistry {
		if c.Mode == commandModeMain && len(c.Keys) == 1 && c.Keys[0] == "B" {
			return
		}
	}
	t.Fatal("commandRegistry has no main-mode B entry")
}
