package cache

// Body is a cached message body: mailstore.Message.Raw (the original header
// and the text parts, no attachment bodies) and Message.TextClipped.
type Body struct {
	Raw         []byte
	TextClipped bool
}
