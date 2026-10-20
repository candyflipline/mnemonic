package cache

type Stats struct {
	LakeFiles              int
	Events, Docs, Postings int64
	Path                   string
}
