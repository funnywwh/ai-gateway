package proxy

import (
	"io"
	"net/http"
	"strings"
)

// stripWorkerCookies also covers trailers populated only after body EOF. Do
// not wrap the 101 body: ReverseProxy needs its io.ReadWriteCloser for upgrades.
func stripWorkerCookies(resp *http.Response) {
	resp.Header.Del("Set-Cookie")
	resp.Header.Del("Set-Cookie2")
	strip := func() {
		resp.Trailer.Del("Set-Cookie")
		resp.Trailer.Del("Set-Cookie2")
	}
	strip()
	var trailers []string
	for _, line := range resp.Header.Values("Trailer") {
		for _, name := range strings.Split(line, ",") {
			name = strings.TrimSpace(name)
			if name != "" && !strings.EqualFold(name, "Set-Cookie") && !strings.EqualFold(name, "Set-Cookie2") {
				trailers = append(trailers, name)
			}
		}
	}
	resp.Header.Del("Trailer")
	if len(trailers) > 0 {
		resp.Header.Set("Trailer", strings.Join(trailers, ", "))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil {
		resp.Body = &cookieFilteredBody{ReadCloser: resp.Body, strip: strip}
	}
}

type cookieFilteredBody struct {
	io.ReadCloser
	strip func()
}

func (b *cookieFilteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.strip()
	return n, err
}
func (b *cookieFilteredBody) Close() error {
	err := b.ReadCloser.Close()
	b.strip()
	return err
}
