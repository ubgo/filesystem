package share

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCheckPasswordRefusesTamperedCosts: a stored hash whose iteration
// count is outside [1, maxPasswordIterations] matches nothing, so a row
// rewritten to a huge count cannot make every check burn the CPU, and one
// rewritten to zero cannot skip the work.
func TestCheckPasswordRefusesTamperedCosts(t *testing.T) {
	stored, err := hashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(stored, "pw") {
		t.Fatal("the right password was refused")
	}
	parts := strings.Split(stored, passwordSep)
	for _, iter := range []int{0, maxPasswordIterations + 1} {
		parts[hashIterations] = strconv.Itoa(iter)
		if checkPassword(strings.Join(parts, passwordSep), "pw") {
			t.Fatalf("a hash with %d iterations matched", iter)
		}
	}
}

// TestLimiterWithoutAttempts: a limiter allowing no attempts refuses
// every key and tracks none.
func TestLimiterWithoutAttempts(t *testing.T) {
	l := newWindowLimiter(0, time.Minute, MaxTrackedKeys, time.Now)
	if l.Allow("a") || len(l.seen) != 0 {
		t.Fatalf("a limiter of 0 attempts allowed one, tracking %d keys", len(l.seen))
	}
}
