package main

import "testing"

func TestWithinRejectsEscape(t *testing.T) {
	for _, rel := range []string{"../etc/passwd", "a/../../b", "/etc/passwd"} {
		p, err := within("/srv/share", rel)
		if rel == "/etc/passwd" {
			if err != nil || p != "/srv/share/etc/passwd" {
				t.Fatalf("%s → %s, %v", rel, p, err)
			}
			continue
		}
		if err != nil || p == "" || p[:len("/srv/share")] != "/srv/share" {
			t.Fatalf("%s → %s, %v", rel, p, err)
		}
	}
}
