package azure

import "time"

// SetClock replaces the driver's clock, so a test can move past a cached
// user delegation key's expiry without waiting days.
func SetClock(d *Driver, now func() time.Time) { d.now = now }
