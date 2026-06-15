package registry

import (
	"os"
	"testing"
)

func TestSetAndGetCard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stored, err := SetCard(Card{
		Agent:        "backend",
		Description:  "owns API, DB, migrations",
		Areas:        []string{"api", "db", "migrations"},
		ContactAbout: "schema changes, endpoint contracts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.UpdatedAt == 0 {
		t.Fatal("UpdatedAt not stamped")
	}

	got, ok, err := GetCard("backend")
	if err != nil || !ok {
		t.Fatalf("GetCard: ok=%v err=%v", ok, err)
	}
	if got.Description != "owns API, DB, migrations" || len(got.Areas) != 3 ||
		got.ContactAbout != "schema changes, endpoint contracts" {
		t.Fatalf("card mismatch: %+v", got)
	}
}

func TestSetCardFullReplace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	SetCard(Card{Agent: "x", Description: "first", Areas: []string{"a", "b"}, ContactAbout: "old"})
	// Re-publish with fewer fields: omitted fields must be cleared, not merged.
	SetCard(Card{Agent: "x", Description: "second"})

	got, _, _ := GetCard("x")
	if got.Description != "second" {
		t.Fatalf("description not replaced: %q", got.Description)
	}
	if len(got.Areas) != 0 {
		t.Fatalf("areas not cleared on full replace: %v", got.Areas)
	}
	if got.ContactAbout != "" {
		t.Fatalf("contact_about not cleared: %q", got.ContactAbout)
	}
}

func TestGetMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, ok, err := GetCard("ghost")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing card")
	}
}

func TestListCards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Empty registry lists nothing without error.
	if cards, err := ListCards(); err != nil || len(cards) != 0 {
		t.Fatalf("empty list: cards=%v err=%v", cards, err)
	}
	SetCard(Card{Agent: "devops", Description: "deploys"})
	SetCard(Card{Agent: "backend", Description: "api"})
	SetCard(Card{Agent: "frontend", Description: "ui"})

	cards, err := ListCards()
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 3 {
		t.Fatalf("len = %d, want 3", len(cards))
	}
	// Ordered by agent name.
	if cards[0].Agent != "backend" || cards[1].Agent != "devops" || cards[2].Agent != "frontend" {
		t.Fatalf("order wrong: %s, %s, %s", cards[0].Agent, cards[1].Agent, cards[2].Agent)
	}
}

func TestNameWithSlash(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := SetCard(Card{Agent: "team/backend", Description: "x"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetCard("team/backend")
	if err != nil || !ok {
		t.Fatalf("GetCard slash name: ok=%v err=%v", ok, err)
	}
	if got.Agent != "team/backend" {
		t.Fatalf("agent name mangled: %q", got.Agent)
	}
	cards, _ := ListCards()
	if len(cards) != 1 || cards[0].Agent != "team/backend" {
		t.Fatalf("list returned wrong agent: %+v", cards)
	}
}

func TestDeleteCard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	SetCard(Card{Agent: "tmp", Description: "y"})
	if err := DeleteCard("tmp"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := GetCard("tmp"); ok {
		t.Fatal("card still present after delete")
	}
	// Deleting an unknown agent is a no-op.
	if err := DeleteCard("never-existed"); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
}

func TestRejectEmptyAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := SetCard(Card{Agent: "   "}); err == nil {
		t.Fatal("expected error for empty agent")
	}
}

func TestListIgnoresJunk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	SetCard(Card{Agent: "real", Description: "z"})
	// A stray non-json and a half-written temp file must not break listing.
	os.WriteFile(Dir()+"/notes.txt", []byte("hi"), 0644)
	os.WriteFile(Dir()+"/broken.json.tmp", []byte("{partial"), 0644)
	cards, err := ListCards()
	if err != nil || len(cards) != 1 || cards[0].Agent != "real" {
		t.Fatalf("list with junk: cards=%+v err=%v", cards, err)
	}
}
