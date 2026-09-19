package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// A server redirect retains the IdP's cross-site Fetch Metadata through the
// callback, portal and tenant. Commit a document on our callback origin first:
// its new navigation to the same-site portal can pass the unchanged dshgw fence.
// The script is fixed; neither OAuth parameters nor a ticket enter executable JS.
const feishuHandoffScript = `window.location.replace(document.getElementById("feishu-continue").href);`

// redirectFeishuHandoff is used only after a successful same-host cookie handoff.
// CLI/legacy requests keep their 303 contract. The cross-host query-ticket path
// deliberately does not call this helper, and mixed-scheme deployments are not
// silently treated as same-site. This selects a response representation; it is
// not authentication and must never replace the OAuth state/identity checks.
func redirectFeishuHandoff(w http.ResponseWriter, r *http.Request, callbackURL, target string) {
	feishuNoStore(w.Header())
	w.Header().Add("Vary", "Sec-Fetch-Site, Sec-Fetch-Mode, Sec-Fetch-Dest, Origin")
	if !feishuHandoffNeedsDocument(r, callbackURL, target) {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	digest := sha256.Sum256([]byte(feishuHandoffScript))
	w.Header().Del("Location")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+
		base64.StdEncoding.EncodeToString(digest[:])+"'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; object-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>进入 DSH</title></head><body><p>飞书身份已验证，正在进入 DSH…</p><p><a id="feishu-continue" href="`+
		html.EscapeString(target)+`">若未自动跳转，点击继续</a></p><script>`+feishuHandoffScript+`</script></body></html>`)
}

func feishuHandoffNeedsDocument(r *http.Request, callbackURL, target string) bool {
	if r.Method != http.MethodGet || len(r.Header.Values("Origin")) != 0 || len(r.Header.Values("Upgrade")) != 0 {
		return false
	}
	for header, expected := range map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document",
	} {
		values := r.Header.Values(header)
		if len(values) != 1 || values[0] != expected {
			return false
		}
	}
	callback, err := url.Parse(callbackURL)
	if err != nil {
		return false
	}
	portal, err := url.Parse(target)
	if err != nil {
		return false
	}
	return (callback.Scheme == "https" || callback.Scheme == "http") &&
		callback.Scheme == portal.Scheme && callback.Host != "" && portal.Host != "" &&
		callback.User == nil && portal.User == nil && callback.Opaque == "" && portal.Opaque == "" &&
		strings.EqualFold(callback.Hostname(), portal.Hostname()) &&
		callback.RawQuery == "" && callback.Fragment == "" && portal.RawQuery == "" && portal.Fragment == ""
}
