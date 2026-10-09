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

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:8080": true,
		":8080": false, "0.0.0.0:8080": false, "10.0.0.5:8080": false, "example.com:8080": false,
	} {
		if got := loopback(addr); got != want {
			t.Errorf("loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
