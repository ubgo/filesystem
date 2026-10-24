package sftp

// Dead returns a channel closed once the driver has noticed that its
// current connection is gone, so a test can drop a connection and know the
// next call will reconnect rather than race the watcher.
func Dead(d *Driver) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur.dead
}
