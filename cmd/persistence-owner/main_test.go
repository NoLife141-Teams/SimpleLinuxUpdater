package main

import "testing"

func TestRunRejectsInvalidOwnershipArguments(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, args := range [][]string{
		{"persistence-owner"},
		{"persistence-owner", "bad", "1000"},
		{"persistence-owner", "1000", "bad"},
		{"persistence-owner", "-1", "1000"},
	} {
		if err := run(args, getenv); err == nil {
			t.Fatalf("run(%v) succeeded, want validation error", args)
		}
	}
}
