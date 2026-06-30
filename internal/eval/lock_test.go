package eval

import "testing"

func TestLockConfigDir(t *testing.T) {
	dir := t.TempDir()

	unlock, err := lockConfigDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// second lock on the same dir must fail while the first is held.
	if _, err := lockConfigDir(dir); err == nil {
		t.Fatal("second lock succeeded; expected it to fail while held")
	}

	unlock()

	// after release, locking again must work.
	unlock2, err := lockConfigDir(dir)
	if err != nil {
		t.Fatalf("re-lock after unlock: %v", err)
	}
	unlock2()
}
