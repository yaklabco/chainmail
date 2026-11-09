package google

// SetSyncLimits lowers the bytes and events one listing may read, so a test
// can reach them with small pages.
func SetSyncLimits(s *Store, bytes, events int) {
	s.syncBytes, s.syncEvents = bytes, events
}
