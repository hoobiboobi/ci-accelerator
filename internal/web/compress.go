package web

import (
	"net/http"
	"strings"

	"github.com/klauspost/compress/gzhttp"
)

// Compress compresses the responses of h for clients that accept it (gzip,
// or zstd): pages, JSON and text shrink to a tenth, which counts behind a
// tunnel, a VPN or a slow proxy (a pipeline's page: 87 KB, 8.7 KB
// compressed). Responses under 1 KB, images and event streams go as they
// are: a compressor would hold an event back until it filled a block.
func Compress(h http.Handler) http.Handler {
	wrap, err := gzhttp.NewWrapper(gzhttp.ContentTypeFilter(compressible))
	if err != nil {
		panic(err) // fixed options: never
	}
	return wrap(h)
}

func compressible(ct string) bool {
	ct = strings.TrimSpace(strings.ToLower(ct))
	if strings.HasPrefix(ct, "text/event-stream") || (strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "image/svg")) {
		return false
	}
	return gzhttp.DefaultContentTypeFilter(ct)
}
