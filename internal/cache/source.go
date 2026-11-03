package cache

// Sources say where an answer came from, as the API reports it: the
// cache, the mail server, or both, for an answer built from parts.
const (
	SourceCache  = "cache"
	SourceServer = "server"
	SourceMixed  = "mixed"
)

// CombineSource is the source of an answer built from parts with the given
// sources: the one source they share, SourceMixed when they differ, and
// SourceServer when there are none. Empty sources are skipped.
func CombineSource(sources ...string) string {
	out := ""
	for _, s := range sources {
		switch {
		case s == "":
		case out == "":
			out = s
		case s != out:
			return SourceMixed
		}
	}
	if out == "" {
		return SourceServer
	}
	return out
}
