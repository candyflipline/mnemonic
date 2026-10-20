package config

// LoadClient reads the client file, expands ~, fills Machine from the short
// hostname and Sources from DefaultSources when empty, reads TokenFile, and
// rejects a missing server or token. Owned by lane push.
func LoadClient(path string) (Client, error) { panic("lane push") }
