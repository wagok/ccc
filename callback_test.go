package main

import (
	"reflect"
	"testing"
)

func TestMultiSelectSubmitKeys(t *testing.T) {
	// Go(0)+Rust(2) of 3 options — the manually verified sequence.
	got := multiSelectSubmitKeys([]bool{true, false, true}, 3)
	want := []string{"Enter", "Down", "Down", "Enter", "Right", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go+Rust = %v, want %v", got, want)
	}
	// First option only.
	if got := multiSelectSubmitKeys([]bool{true, false, false}, 3); !reflect.DeepEqual(got, []string{"Enter", "Down", "Down", "Right", "Enter"}) {
		t.Fatalf("first only = %v", got)
	}
	// Last option only.
	if got := multiSelectSubmitKeys([]bool{false, false, true}, 3); !reflect.DeepEqual(got, []string{"Down", "Down", "Enter", "Right", "Enter"}) {
		t.Fatalf("last only = %v", got)
	}
	// Nothing selected — just open Submit tab and confirm.
	if got := multiSelectSubmitKeys([]bool{false, false, false}, 3); !reflect.DeepEqual(got, []string{"Down", "Down", "Right", "Enter"}) {
		t.Fatalf("none = %v", got)
	}
	// All selected.
	if got := multiSelectSubmitKeys([]bool{true, true}, 2); !reflect.DeepEqual(got, []string{"Enter", "Down", "Enter", "Right", "Enter"}) {
		t.Fatalf("all = %v", got)
	}
}
