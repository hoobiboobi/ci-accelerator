package collector

// connect marks a daemon connected by a new stream with no source and
// returns the stream's number (tests only).
func (l *live) connect(name string) uint64 { return l.connectSource(name, nil) }
