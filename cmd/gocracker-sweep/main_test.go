package main

import "testing"

func TestSelectCasesExcludesIDsAfterSelection(t *testing.T) {
	all := []manifestCase{
		{ID: "alpha", Kind: "dockerfile"},
		{ID: "beta", Kind: "compose"},
		{ID: "gamma", Kind: "dockerfile"},
	}
	ids := map[string]struct{}{
		"alpha": {},
		"beta":  {},
		"gamma": {},
	}
	excluded := map[string]struct{}{
		"beta": {},
	}

	selected := selectCases(all, ids, excluded, "", "", 0, 0, 1, nil)
	if len(selected) != 2 {
		t.Fatalf("selected len = %d, want 2", len(selected))
	}
	if selected[0].ID != "alpha" || selected[1].ID != "gamma" {
		t.Fatalf("selected ids = %#v, want [alpha gamma]", []string{selected[0].ID, selected[1].ID})
	}
}

func TestSelectCasesExclusionOverridesTTYManifest(t *testing.T) {
	all := []manifestCase{
		{ID: "alpha", Kind: "dockerfile"},
		{ID: "beta", Kind: "compose"},
	}
	tty := map[string]*ttyProbe{
		"alpha": {ID: "alpha", Command: "echo ok", Expect: "ok"},
		"beta":  {ID: "beta", Command: "echo ok", Expect: "ok"},
	}
	excluded := map[string]struct{}{
		"beta": {},
	}

	selected := selectCases(all, nil, excluded, "", "", 0, 0, 1, tty)
	if len(selected) != 1 {
		t.Fatalf("selected len = %d, want 1", len(selected))
	}
	if selected[0].ID != "alpha" {
		t.Fatalf("selected id = %q, want alpha", selected[0].ID)
	}
}
