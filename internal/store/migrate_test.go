package store

import "testing"

func TestPurgeRetiredTransports(t *testing.T) {
	s := openTest(t)
	for _, p := range []string{"telegram", "instagram", "Discord", "viber", "whatsapp"} {
		if _, err := s.db.Exec(`INSERT INTO allowlist(platform, handle) VALUES(?, 'x')`, p); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgeRetiredTransports()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("removed %d rows, want 3", n)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM allowlist`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Fatalf("%d rows left, want 2", left)
	}
	if n, _ := s.PurgeRetiredTransports(); n != 0 {
		t.Fatalf("second run removed %d", n)
	}
}
