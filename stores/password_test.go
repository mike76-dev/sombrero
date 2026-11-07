package stores

import "testing"

// TestCheckPassword verifies that an account answers to its own password and
// to nothing else, and that a guest account answers to the empty one.
func TestCheckPassword(t *testing.T) {
	acc := Account{NTHash: ntHash("hunter2")}
	if !acc.CheckPassword("hunter2") {
		t.Error("the right password was refused")
	}
	for _, wrong := range []string{"hunter3", "", "Hunter2"} {
		if acc.CheckPassword(wrong) {
			t.Errorf("password %q was accepted", wrong)
		}
	}

	guest := Account{NTHash: ntHash("")}
	if !guest.CheckPassword("") {
		t.Error("a guest account refused the empty password")
	}
	if guest.CheckPassword("x") {
		t.Error("a guest account accepted a password")
	}

	// An account that was never hashed is not one that anybody may log in as.
	if (Account{}).CheckPassword("") {
		t.Error("an account without a hash accepted the empty password")
	}
}
