package dashboard

import "io"

// closeHTTPBody closes an HTTP request or response body after the caller has
// already handled the operation that matters. Close errors on these bodies do
// not provide actionable recovery after a response has been read or rejected.
func closeHTTPBody(body io.Closer) {
	_ = body.Close()
}
