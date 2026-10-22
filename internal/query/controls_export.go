package query

// ExportHidden is the set of sessions an operator export on the server's data
// dir must not emit: every deleted session, and every private one unless
// includePrivate. The operator sees every author, so tenancy plays no part.
func (c *Controls) ExportHidden(includePrivate bool) map[string]bool {
	sets := c.sets()
	hidden := make(map[string]bool, len(sets.deleted)+len(sets.private))
	for _, id := range sets.deleted {
		hidden[id] = true
	}
	if !includePrivate {
		for _, id := range sets.private {
			hidden[id] = true
		}
	}
	return hidden
}
